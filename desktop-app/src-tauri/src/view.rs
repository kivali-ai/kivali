//! What the bundled pages, the tray and the menus show: the snapshot
//! (`src/types.ts` names the same keys) and the pure decisions behind
//! it: a team's state and phrase, an operation's stages and progress,
//! whether a team fits in memory.

use crate::supervisor::wire::{stage, OrgState, StageProgress, Status};
use serde::Serialize;
use std::time::Duration;

/// A team's state: running, starting, paused, couldn't start.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum TeamState {
    Running,
    Starting,
    Paused,
    Failed,
}

/// What a starting team is doing.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Activity {
    Creating,
    Waking,
    Pausing,
    Updating,
    Applying,
    Deleting,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum OpKind {
    Create,
    Resume,
    Pause,
    Update,
    /// Other devices' address handed to the running server.
    Address,
    Delete,
}

impl OpKind {
    pub fn activity(self) -> Activity {
        match self {
            OpKind::Create => Activity::Creating,
            OpKind::Resume => Activity::Waking,
            OpKind::Pause => Activity::Pausing,
            OpKind::Update => Activity::Updating,
            OpKind::Address => Activity::Applying,
            OpKind::Delete => Activity::Deleting,
        }
    }

    /// The stages of this kind of operation, in the order the supervisor
    /// goes through them: its stage id, the words people see, and a
    /// typical length (for the bar and "about N seconds").
    pub fn stages(self) -> &'static [(&'static str, &'static str, u64)] {
        match self {
            // A new team's first stage makes its data disk and boots
            // the VM for the first time, which formats the disk.
            OpKind::Create => &[
                ("making-room", "Preparing storage", 12),
                ("starting", "Starting up", 20),
                ("setting-up", "Setting up your team", 25),
                ("ready", "Ready", 0),
            ],
            // `up` moves a data disk an earlier version kept elsewhere,
            // then compacts the disk, then boots.
            OpKind::Resume => &[
                ("updating-data", "Updating team data", 120),
                ("making-room", "Making room", 20),
                ("starting", "Starting up", 12),
                ("setting-up", "Setting up your team", 18),
            ],
            // An update that fails goes back to the version it was on.
            // The snapshot's length grows with the data disk where the
            // snapshot copies it ([`lengths`]).
            OpKind::Update => &[
                ("downloading", "Downloading", 60),
                ("snapshot", "Saving a snapshot", 30),
                ("installing", "Installing", 50),
                ("starting", "Starting up", 40),
                ("starting-agents", "Starting your agents", 60),
                ("rolling-back", "Going back to the previous version", 60),
            ],
            OpKind::Pause => &[("pausing", "Pausing", 15)],
            OpKind::Address => &[("setting-up", "Applying the change", 30)],
            OpKind::Delete => &[("deleting", "Deleting", 20)],
        }
    }

    /// The stages an operation of this kind goes through only sometimes
    /// (a resume moves the data disk once, and compacts it only when the
    /// guest has freed enough; an update goes back only when it fails):
    /// they are shown, and count toward the bar, only once the operation
    /// is in them.
    pub fn optional(self) -> &'static [&'static str] {
        match self {
            OpKind::Resume => &["updating-data", "making-room"],
            OpKind::Update => &["rolling-back"],
            _ => &[],
        }
    }

    /// Where an operation of this kind starts before the supervisor names
    /// a stage: its first stage that every one goes through.
    pub fn first_stage(self) -> usize {
        self.stages().iter().position(|(id, _, _)| !self.optional().contains(id)).unwrap_or(0)
    }
}

/// An operation as the pages show it (types.ts `OpView`).
#[derive(Debug, Clone, Serialize)]
pub struct OpView {
    pub kind: OpKind,
    pub running: bool,
    pub error: Option<String>,
    pub finished: bool,
    pub lines: Vec<String>,
    pub stage: Option<String>,
    pub stage_index: usize,
    pub stages: Vec<String>,
    pub started_ms: u64,
    pub percent: u8,
    pub remaining: Option<String>,
    /// How far the current stage's measurable work has got, while it has
    /// any (the caption under the bar).
    pub stage_progress: Option<StageMeasure>,
}

/// A stage's measured progress as the pages show it: `done` of `total`,
/// in `unit`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
pub struct StageMeasure {
    pub done: u64,
    pub total: u64,
    pub unit: MeasureUnit,
}

/// What a stage's measure counts.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum MeasureUnit {
    /// Bytes: a copy of the data disk, an update's download.
    Bytes,
    /// Agents connected, of those with a pod: an update's last wait.
    Agents,
    /// Units that only make a share: a compaction.
    Share,
}

/// The measure of the stage at `index` of `kind`, from the supervisor's
/// report.
pub fn measure(kind: OpKind, index: usize, p: StageProgress) -> StageMeasure {
    let id = kind.stages().get(index).map(|s| s.0).unwrap_or("");
    let unit = match id {
        stage::UPDATING_DATA | stage::DOWNLOADING | stage::SNAPSHOT => MeasureUnit::Bytes,
        stage::STARTING_AGENTS => MeasureUnit::Agents,
        _ => MeasureUnit::Share,
    };
    StageMeasure { done: p.done.min(p.total), total: p.total, unit }
}

/// How fast the data disk is copied for an update's snapshot where the
/// snapshot is a copy (`platform::UPDATE_COPIES_DATA_DISK`), for the
/// estimate before the copy reports its own rate. Not measured: set so
/// that the copy adds about a minute for a data disk of 4 GB and four
/// for 15 GB, in line with the 4 to 8 minutes updates of such teams
/// took on Windows.
pub const SNAPSHOT_COPY_BYTES_PER_SEC: u64 = 64 << 20;

/// The typical length, in seconds, of each of `kind`'s stages in order:
/// [`OpKind::stages`]' own, with an update's snapshot longer by the time
/// to copy `copy_bytes` (the data disk's used bytes where the snapshot
/// copies it, 0 where it is a clone).
pub fn lengths(kind: OpKind, copy_bytes: u64) -> Vec<u64> {
    kind.stages()
        .iter()
        .map(|&(id, _, secs)| if kind == OpKind::Update && id == stage::SNAPSHOT { secs + copy_bytes / SNAPSHOT_COPY_BYTES_PER_SEC } else { secs })
        .collect()
}

/// Where an operation is: which stage, from the supervisor's stage id
/// (an id this kind does not know keeps the index it had), how long it
/// has spent in it.
pub fn stage_index(kind: OpKind, stage_id: Option<&str>) -> Option<usize> {
    let id = stage_id?;
    kind.stages().iter().position(|(s, _, _)| *s == id)
}

/// The stages an operation shows and counts, by index: those it has
/// been in (`seen`), the one it is in, and those still ahead that every
/// operation of its kind goes through (OpKind::optional ones only once
/// they are seen).
pub fn shown_stages(kind: OpKind, seen: &[bool], index: usize) -> Vec<usize> {
    let optional = kind.optional();
    kind.stages()
        .iter()
        .enumerate()
        .filter(|(i, (id, _, _))| seen.get(*i).copied().unwrap_or(false) || *i == index || (*i > index && !optional.contains(id)))
        .map(|(i, _)| i)
        .collect()
}

/// Rough share done and time left, over the stages shown: those before
/// the current one count whole, those after it not at all; the current
/// one by its measured progress when it has some (and the time left from
/// its rate so far), else by time spent against its typical length
/// (never past 95% of it, so a slow stage does not read as done).
/// `lengths` are the stages' typical lengths ([`lengths`]).
pub fn progress(lengths: &[u64], shown: &[usize], index: usize, in_stage: Duration, measured: Option<StageProgress>, finished: bool) -> (u8, Option<String>) {
    if finished {
        return (100, None);
    }
    let secs = |i: usize| lengths.get(i).copied().unwrap_or(0) as f64;
    let total = shown.iter().map(|&i| secs(i)).sum::<f64>().max(1.0);
    let before: f64 = shown.iter().filter(|&&i| i < index).map(|&i| secs(i)).sum();
    let after: f64 = shown.iter().filter(|&&i| i > index).map(|&i| secs(i)).sum();
    let typical = secs(index);
    let spent = in_stage.as_secs_f64();
    let (done, left) = match measured.filter(|m| m.total > 0) {
        Some(m) => {
            let f = (m.done as f64 / m.total as f64).clamp(0.0, 1.0);
            // Under a twentieth done, the rate so far says little.
            let left = if f >= 0.05 { spent * (1.0 - f) / f } else { typical * (1.0 - f) };
            (typical * f, left)
        }
        None => {
            let spent = spent.min(typical * 0.95);
            (spent, typical - spent)
        }
    };
    let percent = (((before + done) / total) * 100.0).clamp(0.0, 99.0) as u8;
    (percent, Some(about(left.max(0.0) + after)))
}

/// "about 40 seconds", "about 3 minutes".
pub fn about(secs: f64) -> String {
    let s = secs.round() as u64;
    if s < 10 {
        "a few seconds".into()
    } else if s < 90 {
        format!("about {} seconds", (s.div_ceil(10)) * 10)
    } else {
        format!("about {} minutes", s.div_ceil(60))
    }
}

/// A team on this computer: the operation running (if any), the last
/// failure to start, and the supervisor's report, to one state.
pub fn local_state(op: Option<OpKind>, failed: bool, status: Option<&Status>) -> (TeamState, Option<Activity>) {
    if let Some(k) = op {
        return (TeamState::Starting, Some(k.activity()));
    }
    if failed {
        return (TeamState::Failed, None);
    }
    match status.map(|s| s.state) {
        Some(OrgState::Running) => (TeamState::Running, None),
        Some(OrgState::Starting) => (TeamState::Starting, Some(Activity::Waking)),
        Some(OrgState::Upgrading) => (TeamState::Starting, Some(Activity::Updating)),
        Some(OrgState::Failed) => (TeamState::Failed, None),
        Some(OrgState::Stopped | OrgState::Absent) | None => (TeamState::Paused, None),
    }
}

/// The short phrase beside a team's dot (tray rows, Settings). `working`
/// is how many agents the team says are working (teamapi.rs), when known.
pub fn phrase(state: TeamState, activity: Option<Activity>, signed_in: Option<bool>, elsewhere: Option<&str>, working: Option<u64>) -> String {
    if let Some(device) = elsewhere {
        return match state {
            TeamState::Running => format!("on {device}"),
            _ => format!("can't reach {device}"),
        };
    }
    match (state, activity) {
        (TeamState::Running, _) if signed_in == Some(false) => "Claude isn't signed in".into(),
        (TeamState::Running, _) => match working {
            Some(1) => "1 agent working".into(),
            Some(n) if n > 1 => format!("{n} agents working"),
            _ => "running".into(),
        },
        (TeamState::Starting, Some(Activity::Creating)) => "getting ready".into(),
        (TeamState::Starting, Some(Activity::Pausing)) => "pausing".into(),
        (TeamState::Starting, Some(Activity::Updating)) => "updating".into(),
        (TeamState::Starting, Some(Activity::Applying)) => "applying a change".into(),
        (TeamState::Starting, Some(Activity::Deleting)) => "deleting".into(),
        (TeamState::Starting, _) => "waking up".into(),
        (TeamState::Paused, _) => "paused".into(),
        (TeamState::Failed, _) => "couldn't start".into(),
    }
}

/// The pause dialog's sentence: how many agents a pause interrupts, when the team said.
pub fn pause_message(working: Option<u64>) -> String {
    match working {
        Some(0) => "No agents are working right now. They pick up where they left off when you resume.".into(),
        Some(1) => "1 agent is working. It stops mid-task and picks up where it left off when you resume.".into(),
        Some(n) => format!("{n} agents are working. They stop mid-task and pick up where they left off when you resume."),
        None => "Agents that are working stop mid-task and pick up where they left off when you resume.".into(),
    }
}

/// How long an operation of this kind typically takes, in words, over
/// the stages every one goes through: "about 4 minutes" for an update
/// whose snapshot copies nothing; `copy_bytes` as for [`lengths`].
pub fn typical(kind: OpKind, copy_bytes: u64) -> String {
    let optional = kind.optional();
    let secs: u64 = kind.stages().iter().zip(lengths(kind, copy_bytes)).filter(|(s, _)| !optional.contains(&s.0)).map(|(_, l)| l).sum();
    about(secs as f64)
}

/// Memory a team may use on this computer: what is left of the host's
/// after the system's share (a quarter, at least 4 GB) and the teams
/// already running.
pub fn memory_free_mb(host_mb: u64, running_mb: u64) -> u64 {
    let reserve = (host_mb / 4).max(4096);
    host_mb.saturating_sub(reserve).saturating_sub(running_mb)
}

/// "4 GB" for a memory size in MiB.
/// `s` with its first letter upper-cased, to start a sentence with
/// platform wording ("This PC").
pub fn capitalized(s: &str) -> String {
    let mut c = s.chars();
    c.next().map(|f| f.to_uppercase().chain(c).collect()).unwrap_or_default()
}

/// Why a team here can't start while this computer can't run its VM, in
/// one sentence; None when it can. The pages add the steps
/// (logic/vmhelp.ts).
pub fn vm_reason(v: crate::platform::VmSupport) -> Option<String> {
    use crate::platform::VmSupport::*;
    match v {
        Ready => None,
        HypervOff => Some("Hyper-V is turned off on this PC.".into()),
        WindowsHome => Some("Teams on this PC run in Hyper-V, which Windows Home doesn't include.".into()),
        VirtualizationOff => Some("Hyper-V can't run because virtualization is turned off on this PC.".into()),
        InVm => Some("This Mac can't run virtual machines.".into()),
    }
}

pub fn gb(mb: u64) -> String {
    let g = mb as f64 / 1024.0;
    if (g - g.round()).abs() < 0.05 {
        format!("{} GB", g.round() as u64)
    } else {
        format!("{g:.1} GB")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn capitalized_starts_a_sentence() {
        assert_eq!(capitalized("this PC"), "This PC");
        assert_eq!(capitalized("this Mac"), "This Mac");
        assert_eq!(capitalized(""), "");
    }

    #[test]
    fn vm_reasons_are_sentences_and_ready_has_none() {
        use crate::platform::VmSupport::*;
        assert_eq!(vm_reason(Ready), None);
        for v in [HypervOff, WindowsHome, VirtualizationOff, InVm] {
            let r = vm_reason(v).unwrap();
            assert!(r.ends_with('.') && r.chars().next().unwrap().is_uppercase(), "{r}");
        }
    }

    fn st(s: OrgState) -> Status {
        Status { state: s, port: None, kivali: None, message: None }
    }

    #[test]
    fn states() {
        assert_eq!(local_state(Some(OpKind::Pause), false, Some(&st(OrgState::Running))), (TeamState::Starting, Some(Activity::Pausing)));
        assert_eq!(local_state(None, true, Some(&st(OrgState::Running))), (TeamState::Failed, None));
        assert_eq!(local_state(None, false, Some(&st(OrgState::Running))), (TeamState::Running, None));
        assert_eq!(local_state(None, false, Some(&st(OrgState::Upgrading))).1, Some(Activity::Updating));
        assert_eq!(local_state(None, false, None), (TeamState::Paused, None));
        assert_eq!(local_state(None, false, Some(&st(OrgState::Failed))).0, TeamState::Failed);
    }

    #[test]
    fn phrases() {
        assert_eq!(phrase(TeamState::Running, None, Some(true), None, None), "running");
        assert_eq!(phrase(TeamState::Running, None, Some(false), None, Some(3)), "Claude isn't signed in");
        assert_eq!(phrase(TeamState::Starting, Some(Activity::Updating), None, None, Some(3)), "updating");
        assert_eq!(phrase(TeamState::Paused, None, None, None, None), "paused");
        assert_eq!(phrase(TeamState::Failed, None, None, None, None), "couldn't start");
        assert_eq!(phrase(TeamState::Running, None, None, Some("dana-imac.local"), Some(3)), "on dana-imac.local");
        assert_eq!(phrase(TeamState::Paused, None, None, Some("dana-imac.local"), None), "can't reach dana-imac.local");
        // The tray's "Plainsong · 3 agents working".
        assert_eq!(phrase(TeamState::Running, None, Some(true), None, Some(3)), "3 agents working");
        assert_eq!(phrase(TeamState::Running, None, None, None, Some(1)), "1 agent working");
        assert_eq!(phrase(TeamState::Running, None, Some(true), None, Some(0)), "running");
    }

    #[test]
    fn dialog_words() {
        assert_eq!(pause_message(Some(3)), "3 agents are working. They stop mid-task and pick up where they left off when you resume.");
        assert_eq!(pause_message(Some(1)), "1 agent is working. It stops mid-task and picks up where it left off when you resume.");
        assert!(pause_message(Some(0)).starts_with("No agents are working right now."));
        assert!(pause_message(None).starts_with("Agents that are working stop mid-task"));
        // The update dialog: "It takes about 4 minutes."
        assert_eq!(typical(OpKind::Update, 0), "about 4 minutes");
    }

    #[test]
    fn stages_and_progress() {
        assert_eq!(stage_index(OpKind::Create, Some("starting")), Some(1));
        assert_eq!(stage_index(OpKind::Create, Some("bogus")), None);
        assert_eq!(stage_index(OpKind::Update, Some("snapshot")), Some(1));
        let all = |kind: OpKind| (0..kind.stages().len()).collect::<Vec<_>>();
        let (p, left) = progress(&lengths(OpKind::Create, 0), &all(OpKind::Create), 1, Duration::from_secs(10), None, false);
        // 12 + 10 of 57 seconds.
        assert_eq!(p, 38);
        assert_eq!(left.as_deref(), Some("about 40 seconds"));
        // A slow stage never reads as past 95% of itself.
        let (p, _) = progress(&lengths(OpKind::Pause, 0), &[0], 0, Duration::from_secs(600), None, false);
        assert_eq!(p, 95);
        assert_eq!(progress(&lengths(OpKind::Create, 0), &all(OpKind::Create), 2, Duration::ZERO, None, true), (100, None));
        assert_eq!(about(170.0), "about 3 minutes");
        assert_eq!(about(4.0), "a few seconds");
    }

    /// A resume's stages are in the order `up` goes through them; moving
    /// the data disk and compacting it are shown, and counted, only once
    /// the resume is in them, so a plain resume reads as before.
    #[test]
    fn resume_stages() {
        let ids: Vec<_> = OpKind::Resume.stages().iter().map(|s| s.0).collect();
        assert_eq!(ids, ["updating-data", "making-room", "starting", "setting-up"]);
        assert_eq!(OpKind::Resume.first_stage(), 2);
        assert_eq!(OpKind::Create.first_stage(), 0);
        assert_eq!(typical(OpKind::Resume, 0), "about 30 seconds");

        // A plain resume: starting, then setting up, 12 and 18 seconds.
        let mut seen = [false, false, true, false];
        assert_eq!(shown_stages(OpKind::Resume, &seen, 2), [2, 3]);
        let (p, left) = progress(&lengths(OpKind::Resume, 0), &[2, 3], 2, Duration::from_secs(6), None, false);
        assert_eq!((p, left.as_deref()), (20, Some("about 30 seconds")));

        // The supervisor moves the disk: every stage from there on shows.
        seen[0] = true;
        assert_eq!(shown_stages(OpKind::Resume, &seen, 0), [0, 2, 3]);
        seen[1] = true;
        assert_eq!(shown_stages(OpKind::Resume, &seen, 1), [0, 1, 2, 3]);
        // Moved without compacting: the stage skipped no longer shows.
        assert_eq!(shown_stages(OpKind::Resume, &[true, false, true, false], 2), [0, 2, 3]);
    }

    /// A stage that reports its progress fills its share of the bar by
    /// what it has done, not by time, and its time left comes from its
    /// rate so far.
    #[test]
    fn measured_progress_overrides_the_estimate() {
        let shown = [0, 2, 3]; // updating-data 120, starting 12, setting-up 18
        let three_of_eight = Some(StageProgress { done: 3 << 30, total: 8 << 30 });
        // 3/8 of 120 seconds' share = 45 of 150.
        let (p, left) = progress(&lengths(OpKind::Resume, 0), &shown, 0, Duration::from_secs(30), three_of_eight, false);
        assert_eq!(p, 30);
        // 30 s for 3/8: 50 s more, and 30 s of the stages after.
        assert_eq!(left.as_deref(), Some("about 80 seconds"));
        // However long it has taken, done is done.
        let (p, _) = progress(&lengths(OpKind::Resume, 0), &shown, 0, Duration::from_secs(600), Some(StageProgress { done: 8, total: 8 }), false);
        assert_eq!(p, 80);
        // Without a report, time against the typical length, as before.
        let (p, _) = progress(&lengths(OpKind::Resume, 0), &shown, 0, Duration::from_secs(30), None, false);
        assert_eq!(p, 20);
        // A report with no total is no measure.
        let (p, _) = progress(&lengths(OpKind::Resume, 0), &shown, 0, Duration::from_secs(30), Some(StageProgress { done: 0, total: 0 }), false);
        assert_eq!(p, 20);
        // The copy is measured in bytes; a compaction only makes a share.
        assert_eq!(measure(OpKind::Resume, 0, StageProgress { done: 9, total: 8 }), StageMeasure { done: 8, total: 8, unit: MeasureUnit::Bytes });
        assert_eq!(measure(OpKind::Resume, 1, StageProgress { done: 40, total: 100 }), StageMeasure { done: 40, total: 100, unit: MeasureUnit::Share });
    }

    /// An update's download and snapshot report bytes, and its wait for
    /// the agents counts agents, each captioned as such; without it the
    /// caption read "Downloading · 40%" against the supervisor's byte
    /// counts, or nothing at all.
    #[test]
    fn update_measures() {
        let ids: Vec<_> = OpKind::Update.stages().iter().map(|s| s.0).collect();
        assert_eq!(ids, [stage::DOWNLOADING, stage::SNAPSHOT, stage::INSTALLING, stage::STARTING, stage::STARTING_AGENTS, stage::ROLLING_BACK]);
        let p = StageProgress { done: 3 << 30, total: 8 << 30 };
        assert_eq!(measure(OpKind::Update, 0, p).unit, MeasureUnit::Bytes);
        assert_eq!(measure(OpKind::Update, 1, p).unit, MeasureUnit::Bytes);
        assert_eq!(measure(OpKind::Update, 4, StageProgress { done: 3, total: 5 }), StageMeasure { done: 3, total: 5, unit: MeasureUnit::Agents });
    }

    /// A failed update goes back to its version in a stage of its own,
    /// shown only once it is in it. Without it the bar read "Starting up
    /// · step 4 of 4" through the whole rollback.
    #[test]
    fn update_rollback_stage() {
        assert_eq!(OpKind::Update.first_stage(), 0);
        let mut seen = [true, true, true, false, false, false];
        // A plain update never shows it.
        assert_eq!(shown_stages(OpKind::Update, &seen, 2), [0, 1, 2, 3, 4]);
        // A failure while installing goes back from there.
        seen[5] = true;
        assert_eq!(shown_stages(OpKind::Update, &seen, 5), [0, 1, 2, 5]);
        assert_eq!(stage_index(OpKind::Update, Some(stage::ROLLING_BACK)), Some(5));
        assert_eq!(OpKind::Update.stages()[5].1, "Going back to the previous version");
    }

    /// Where the snapshot copies the data disk, the update's length grows
    /// with the disk, and so does the snapshot's share of the bar; a
    /// clone adds nothing; the wait for the agents counts on both.
    /// Without it the dialog said "about 3 minutes" for an update that
    /// took 4 to 8 on Windows.
    #[test]
    fn update_length_follows_the_copy() {
        assert_eq!(typical(OpKind::Update, 0), "about 4 minutes");
        assert_eq!(typical(OpKind::Update, 4 << 30), "about 6 minutes");
        assert_eq!(typical(OpKind::Update, 15 << 30), "about 8 minutes");
        assert_eq!(lengths(OpKind::Update, 4 << 30), [60, 94, 50, 40, 60, 60]);
        // Only an update's snapshot grows.
        assert_eq!(lengths(OpKind::Resume, 4 << 30), lengths(OpKind::Resume, 0));
        // Halfway through a 4 GB copy: 60 + 47 of 304 seconds.
        let half = Some(StageProgress { done: 2 << 30, total: 4 << 30 });
        let (p, _) = progress(&lengths(OpKind::Update, 4 << 30), &[0, 1, 2, 3, 4], 1, Duration::from_secs(47), half, false);
        assert_eq!(p, 35);
    }

    #[test]
    fn memory() {
        // 16 GB: 4 kept for the system.
        assert_eq!(memory_free_mb(16384, 4096), 8192);
        assert_eq!(memory_free_mb(8192, 4096), 0);
        assert_eq!(memory_free_mb(65536, 0), 49152);
        assert_eq!(gb(4096), "4 GB");
        assert_eq!(gb(1536), "1.5 GB");
    }
}
