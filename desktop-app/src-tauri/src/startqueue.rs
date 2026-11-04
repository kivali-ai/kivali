//! Teams' starts on this computer, one at a time. Hyper-V's Dynamic
//! Memory backs a VM's whole startup memory when it powers on, so two
//! teams powering on together can be refused for memory although the
//! shell's estimate (`view::memory_free_mb`) has room for both. Every
//! resume takes a place in the [`StartQueue`] and boots only once the
//! start ahead of it has ended, however it ended ([`start_in_turn`]). A
//! start refused for memory ([`out_of_memory`]) is tried once more,
//! [`SETTLE`] later, still in its turn: by then the VMs started before it
//! have given back what they held to boot. Virtualization.framework
//! reserves no memory at power-on, so on macOS the second try never
//! comes up; the queue is the same on both.

use std::collections::VecDeque;
use std::sync::{Arc, Condvar, Mutex, MutexGuard};
use std::time::{Duration, Instant};

/// How long a start refused for memory waits before its one retry.
pub const SETTLE: Duration = Duration::from_secs(60);

/// What a refusal for memory says, lower-cased: Hyper-V's "Not enough
/// memory in the system to start the virtual machine", and Windows'
/// "Not enough memory resources are available" behind it.
const MEMORY_WORDS: &[&str] = &["not enough memory"];

/// Whether a start's error is the host refusing the VM its memory.
pub fn out_of_memory(err: &str) -> bool {
    let e = err.to_lowercase();
    MEMORY_WORDS.iter().any(|w| e.contains(w))
}

/// The time, injected so tests move it instead of waiting.
pub trait Clock: Send + Sync {
    fn now(&self) -> Instant;
    /// Waits on `cv` for at most `d`, as `Condvar::wait_timeout` does.
    fn wait<'a>(&self, cv: &Condvar, line: MutexGuard<'a, Line>, d: Duration) -> MutexGuard<'a, Line>;
}

pub struct SystemClock;

impl Clock for SystemClock {
    fn now(&self) -> Instant {
        Instant::now()
    }

    fn wait<'a>(&self, cv: &Condvar, line: MutexGuard<'a, Line>, d: Duration) -> MutexGuard<'a, Line> {
        cv.wait_timeout(line, d).unwrap_or_else(|e| e.into_inner()).0
    }
}

/// The starts in line, first the one whose turn it is.
#[derive(Default)]
pub struct Line {
    next: u64,
    places: VecDeque<Place>,
    closed: bool,
}

struct Place {
    n: u64,
    id: String,
    /// Its turn came and its start began.
    began: bool,
}

/// One start at a time, in the order they joined.
pub struct StartQueue {
    line: Mutex<Line>,
    turn: Condvar,
    clock: Box<dyn Clock>,
}

/// A start's place in the queue; dropping it leaves the queue, and the
/// next start's turn comes.
pub struct Ticket {
    queue: Arc<StartQueue>,
    n: u64,
}

impl StartQueue {
    pub fn new(clock: Box<dyn Clock>) -> Arc<StartQueue> {
        Arc::new(StartQueue { line: Mutex::new(Line::default()), turn: Condvar::new(), clock })
    }

    fn lock(&self) -> MutexGuard<'_, Line> {
        self.line.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// A place at the back for team `id`'s start.
    pub fn join(self: &Arc<Self>, id: &str) -> Ticket {
        let mut line = self.lock();
        let n = line.next;
        line.next += 1;
        line.places.push_back(Place { n, id: id.to_string(), began: false });
        Ticket { queue: self.clone(), n }
    }

    /// Quitting: the starts still waiting give up, and so does one
    /// waiting to try again. Answers the teams whose start had not begun.
    pub fn close(&self) -> Vec<String> {
        let mut line = self.lock();
        line.closed = true;
        let waiting = line.places.iter().filter(|p| !p.began).map(|p| p.id.clone()).collect();
        drop(line);
        self.turn.notify_all();
        waiting
    }

    /// Undoes [`Self::close`] (an app update whose installer could not
    /// start).
    pub fn reopen(&self) {
        self.lock().closed = false;
    }
}

impl Ticket {
    /// Blocks until this start's turn; `ahead` hears the team whose start
    /// runs meanwhile, each time that changes. False once the queue is
    /// closed.
    pub fn wait_turn(&self, ahead: &mut dyn FnMut(&str)) -> bool {
        let q = &self.queue;
        let mut told = None;
        let mut line = q.lock();
        loop {
            if line.closed {
                return false;
            }
            let first = line.places.front().map(|p| (p.n, p.id.clone()));
            match first {
                Some((n, _)) if n == self.n => {
                    if let Some(p) = line.places.front_mut() {
                        p.began = true;
                    }
                    return true;
                }
                Some((n, id)) if told != Some(n) => {
                    told = Some(n);
                    // Not under the lock: what hears it may redraw.
                    drop(line);
                    ahead(&id);
                    line = q.lock();
                }
                _ => line = q.turn.wait(line).unwrap_or_else(|e| e.into_inner()),
            }
        }
    }

    /// Waits `d` on the queue's clock, keeping the turn. False once the
    /// queue is closed.
    pub fn settle(&self, d: Duration) -> bool {
        let q = &self.queue;
        let deadline = q.clock.now() + d;
        let mut line = q.lock();
        loop {
            if line.closed {
                return false;
            }
            let now = q.clock.now();
            if now >= deadline {
                return true;
            }
            line = q.clock.wait(&q.turn, line, deadline - now);
        }
    }
}

impl Drop for Ticket {
    fn drop(&mut self) {
        self.queue.lock().places.retain(|p| p.n != self.n);
        self.queue.turn.notify_all();
    }
}

/// What one start does, as [`start_in_turn`] drives it.
pub trait Starter {
    /// Still waiting: team `ahead` is starting.
    fn waiting(&mut self, ahead: &str);
    /// The start was refused for memory and is tried again after [`SETTLE`].
    fn retrying(&mut self);
    /// Boots the team.
    fn start(&mut self) -> Result<(), String>;
}

/// How a start in turn ended.
#[derive(Debug, PartialEq, Eq)]
pub enum Ended {
    Started,
    Failed(String),
    /// Refused for memory twice: the second refusal.
    NoMemory(String),
    /// The queue closed before the start (or its retry) could run.
    Closed,
}

/// Runs one start in its turn: waits for the starts ahead, then starts;
/// one refused for memory waits [`SETTLE`] and starts once more.
pub fn start_in_turn(ticket: &Ticket, s: &mut dyn Starter) -> Ended {
    if !ticket.wait_turn(&mut |ahead| s.waiting(ahead)) {
        return Ended::Closed;
    }
    let first = match s.start() {
        Ok(()) => return Ended::Started,
        Err(e) => e,
    };
    if !out_of_memory(&first) {
        return Ended::Failed(first);
    }
    s.retrying();
    if !ticket.settle(SETTLE) {
        return Ended::Closed;
    }
    match s.start() {
        Ok(()) => Ended::Started,
        Err(e) if out_of_memory(&e) => Ended::NoMemory(e),
        Err(e) => Ended::Failed(e),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    /// Time that moves only when a wait asks it to.
    struct TestClock {
        now: Mutex<Instant>,
    }

    impl Clock for Arc<TestClock> {
        fn now(&self) -> Instant {
            *self.now.lock().unwrap()
        }

        fn wait<'a>(&self, _cv: &Condvar, line: MutexGuard<'a, Line>, d: Duration) -> MutexGuard<'a, Line> {
            *self.now.lock().unwrap() += d;
            line
        }
    }

    fn queue() -> (Arc<StartQueue>, Arc<TestClock>) {
        let clock = Arc::new(TestClock { now: Mutex::new(Instant::now()) });
        (StartQueue::new(Box::new(clock.clone())), clock)
    }

    const HYPERV_REFUSAL: &str = "start the VM: 'kivali-team-0001' failed to start. Not enough memory in the system to start the virtual machine kivali-team-0001.";

    /// A starter that answers from a script, logging each start with the
    /// clock's time.
    struct Scripted {
        id: &'static str,
        clock: Arc<TestClock>,
        answers: VecDeque<Result<(), String>>,
        log: Arc<Mutex<Vec<(&'static str, Instant)>>>,
        waited_for: Vec<String>,
        retried: usize,
    }

    impl Scripted {
        fn new(id: &'static str, clock: &Arc<TestClock>, log: &Arc<Mutex<Vec<(&'static str, Instant)>>>, answers: Vec<Result<(), String>>) -> Scripted {
            Scripted { id, clock: clock.clone(), answers: answers.into(), log: log.clone(), waited_for: Vec::new(), retried: 0 }
        }
    }

    impl Starter for Scripted {
        fn waiting(&mut self, ahead: &str) {
            self.waited_for.push(ahead.to_string());
        }
        fn retrying(&mut self) {
            self.retried += 1;
        }
        fn start(&mut self) -> Result<(), String> {
            self.log.lock().unwrap().push((self.id, self.clock.now()));
            self.answers.pop_front().expect("a start the script did not expect")
        }
    }

    #[test]
    fn recognises_a_refusal_for_memory() {
        assert!(out_of_memory(HYPERV_REFUSAL));
        assert!(out_of_memory("Not enough memory resources are available to process this command."));
        assert!(!out_of_memory("the VM did not connect to its console pipe"));
        assert!(!out_of_memory("memory:team-0002"));
    }

    /// Teams resumed together start one at a time, in the order they
    /// joined: each start begins only once the one ahead has ended, and
    /// hears which team it waits for.
    #[test]
    fn starts_run_one_at_a_time_in_order() {
        let (q, clock) = queue();
        let log = Arc::new(Mutex::new(Vec::new()));
        let in_flight = Arc::new(AtomicUsize::new(0));
        // Joined in launch order, on this thread, before any runs.
        let tickets: Vec<_> = ["a", "b", "c"].iter().map(|id| (*id, q.join(id))).collect();

        /// Counts the starts running at once; A's start holds until both
        /// others have heard they wait for it.
        struct Counted {
            inner: Scripted,
            in_flight: Arc<AtomicUsize>,
            heard: std::sync::mpsc::Sender<()>,
            gate: Option<std::sync::mpsc::Receiver<()>>,
        }
        impl Starter for Counted {
            fn waiting(&mut self, ahead: &str) {
                self.inner.waiting(ahead);
                let _ = self.heard.send(());
            }
            fn retrying(&mut self) {
                self.inner.retrying();
            }
            fn start(&mut self) -> Result<(), String> {
                assert_eq!(self.in_flight.fetch_add(1, Ordering::SeqCst), 0, "two starts at once");
                if let Some(gate) = &self.gate {
                    gate.recv().unwrap();
                    gate.recv().unwrap();
                }
                let r = self.inner.start();
                self.in_flight.fetch_sub(1, Ordering::SeqCst);
                r
            }
        }

        // Each waits on its own thread, the last started first.
        let (heard, gate) = std::sync::mpsc::channel();
        let mut gate = Some(gate);
        let handles: Vec<_> = tickets
            .into_iter()
            .rev()
            .map(|(id, ticket)| {
                let mut s = Counted {
                    inner: Scripted::new(id, &clock, &log, vec![Ok(())]),
                    in_flight: in_flight.clone(),
                    heard: heard.clone(),
                    gate: if id == "a" { gate.take() } else { None },
                };
                std::thread::spawn(move || {
                    let ended = start_in_turn(&ticket, &mut s);
                    (id, ended, s.inner.waited_for)
                })
            })
            .collect();
        let mut results: Vec<_> = handles.into_iter().map(|h| h.join().unwrap()).collect();
        results.sort_by_key(|r| r.0);
        let order: Vec<_> = log.lock().unwrap().iter().map(|(id, _)| *id).collect();
        assert_eq!(order, ["a", "b", "c"]);
        for (_, ended, _) in &results {
            assert_eq!(*ended, Ended::Started);
        }
        assert!(results[0].2.is_empty());
        assert_eq!(results[1].2, ["a"]);
        // C waited for A, then (unless B was already done) for B.
        assert_eq!(results[2].2.first().map(String::as_str), Some("a"));
        assert!(results[2].2.iter().all(|w| w == "a" || w == "b"));
    }

    /// A start refused for memory, after the start ahead of it ended,
    /// tries once more SETTLE later, before the team behind it starts.
    #[test]
    fn a_refusal_for_memory_is_tried_again_once() {
        let (q, clock) = queue();
        let log = Arc::new(Mutex::new(Vec::new()));
        let (a, b, c) = (q.join("a"), q.join("b"), q.join("c"));
        let t0 = clock.now();
        assert_eq!(start_in_turn(&a, &mut Scripted::new("a", &clock, &log, vec![Ok(())])), Ended::Started);
        drop(a);
        let mut sb = Scripted::new("b", &clock, &log, vec![Err(HYPERV_REFUSAL.into()), Ok(())]);
        assert_eq!(start_in_turn(&b, &mut sb), Ended::Started);
        assert_eq!(sb.retried, 1);
        drop(b);
        assert_eq!(start_in_turn(&c, &mut Scripted::new("c", &clock, &log, vec![Ok(())])), Ended::Started);
        let log = log.lock().unwrap();
        let at: Vec<_> = log.iter().map(|(id, t)| (*id, *t - t0)).collect();
        assert_eq!(at, [("a", Duration::ZERO), ("b", Duration::ZERO), ("b", SETTLE), ("c", SETTLE)]);
    }

    /// Refused for memory twice: the start ends as such, with the second
    /// refusal, and is not tried a third time.
    #[test]
    fn a_second_refusal_for_memory_ends_the_start() {
        let (q, clock) = queue();
        let log = Arc::new(Mutex::new(Vec::new()));
        let b = q.join("b");
        let mut s = Scripted::new("b", &clock, &log, vec![Err(HYPERV_REFUSAL.into()), Err(format!("{HYPERV_REFUSAL} again"))]);
        assert_eq!(start_in_turn(&b, &mut s), Ended::NoMemory(format!("{HYPERV_REFUSAL} again")));
        assert_eq!(log.lock().unwrap().len(), 2);
        assert_eq!(s.retried, 1);
    }

    /// Any other failure ends the start at once, with no wait and no retry.
    #[test]
    fn other_failures_are_not_tried_again() {
        let (q, clock) = queue();
        let log = Arc::new(Mutex::new(Vec::new()));
        let t0 = clock.now();
        let b = q.join("b");
        let mut s = Scripted::new("b", &clock, &log, vec![Err("the VM did not connect to its console pipe".into())]);
        assert_eq!(start_in_turn(&b, &mut s), Ended::Failed("the VM did not connect to its console pipe".into()));
        assert_eq!(log.lock().unwrap().len(), 1);
        assert_eq!((s.retried, clock.now() - t0), (0, Duration::ZERO));
        // A retry refused for something else ends as that failure.
        let c = q.join("c");
        let mut s = Scripted::new("c", &clock, &log, vec![Err(HYPERV_REFUSAL.into()), Err("the supervisor answered unexpectedly".into())]);
        drop(b);
        assert_eq!(start_in_turn(&c, &mut s), Ended::Failed("the supervisor answered unexpectedly".into()));
    }

    /// Quit closes the queue: the starts still waiting give up and are
    /// named (they resume at the next launch); a ticket dropped unused
    /// (its operation refused) passes the turn on.
    #[test]
    fn closing_ends_the_waiting_starts() {
        let (q, clock) = queue();
        let log = Arc::new(Mutex::new(Vec::new()));
        let unused = q.join("x");
        let (a, b) = (q.join("a"), q.join("b"));
        drop(unused);
        assert!(a.wait_turn(&mut |_| panic!("a is first")));
        assert_eq!(q.close(), ["b"]);
        assert_eq!(start_in_turn(&b, &mut Scripted::new("b", &clock, &log, vec![])), Ended::Closed);
        // A start waiting to try again gives up too.
        assert!(!a.settle(SETTLE));
        assert!(log.lock().unwrap().is_empty());
        drop((a, b));
        q.reopen();
        let c = q.join("c");
        assert_eq!(start_in_turn(&c, &mut Scripted::new("c", &clock, &log, vec![Ok(())])), Ended::Started);
    }
}
