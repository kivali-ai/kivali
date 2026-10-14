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
            OpKind::Create => &[
                ("making-room", "Making room", 12),
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
            OpKind::Update => &[
                ("downloading", "Downloading", 60),
                ("snapshot", "Saving a snapshot", 30),
                ("installing", "Installing", 50),
                ("starting", "Starting up", 40),
            ],
            OpKind::Pause => &[("pausing", "Pausing", 15)],
            OpKind::Address => &[("setting-up", "Applying the change", 30)],
            OpKind::Delete => &[("deleting", "Deleting", 20)],
        }
    }

    /// The stages an operation of this kind goes through only sometimes
    /// (a resume moves the data disk once, and compacts it only when the
    /// guest has freed enough): they are shown, and count toward the bar,
    /// only once the operation is in them.
    pub fn optional(self) -> &'static [&'static str] {
        match self {
            OpKind::Resume => &["updating-data", "making-room"],
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
/// in bytes when `bytes` (the data disk's copy), else in units that
/// only make a share (a compaction).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
pub struct StageMeasure {
    pub done: u64,
    pub total: u64,
    pub bytes: bool,
}

/// The measure of the stage at `index` of `kind`, from the supervisor's
/// report.
pub fn measure(kind: OpKind, index: usize, p: StageProgress) -> StageMeasure {
    let id = kind.stages().get(index).map(|s| s.0).unwrap_or("");
    StageMeasure { done: p.done.min(p.total), total: p.total, bytes: id == stage::UPDATING_DATA }
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
pub fn progress(kind: OpKind, shown: &[usize], index: usize, in_stage: Duration, measured: Option<StageProgress>, finished: bool) -> (u8, Option<String>) {
    if finished {
        return (100, None);
    }
    let stages = kind.stages();
    let secs = |i: usize| stages.get(i).map(|s| s.2).unwrap_or(0) as f64;
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

/// How long an operation of this kind typically takes, in words: "about
/// 3 minutes" for an update.
pub fn typical(kind: OpKind) -> String {
    about(kind.stages().iter().filter(|s| !kind.optional().contains(&s.0)).map(|s| s.2).sum::<u64>() as f64)
}

/// Memory a team may use on this computer: what is left of the host's
/// after the system's share (a quarter, at least 4 GB) and the teams
/// already running.
pub fn memory_free_mb(host_mb: u64, running_mb: u64) -> u64 {
    let reserve = (host_mb / 4).max(4096);
    host_mb.saturating_sub(reserve).saturating_sub(running_mb)
}

/// "4 GB" for a memory size in MiB.
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
        // The update dialog: "It takes about 3 minutes."
        assert_eq!(typical(OpKind::Update), "about 3 minutes");
    }

    #[test]
    fn stages_and_progress() {
        assert_eq!(stage_index(OpKind::Create, Some("starting")), Some(1));
        assert_eq!(stage_index(OpKind::Create, Some("bogus")), None);
        assert_eq!(stage_index(OpKind::Update, Some("snapshot")), Some(1));
        let all = |kind: OpKind| (0..kind.stages().len()).collect::<Vec<_>>();
        let (p, left) = progress(OpKind::Create, &all(OpKind::Create), 1, Duration::from_secs(10), None, false);
        // 12 + 10 of 57 seconds.
        assert_eq!(p, 38);
        assert_eq!(left.as_deref(), Some("about 40 seconds"));
        // A slow stage never reads as past 95% of itself.
        let (p, _) = progress(OpKind::Pause, &[0], 0, Duration::from_secs(600), None, false);
        assert_eq!(p, 95);
        assert_eq!(progress(OpKind::Create, &all(OpKind::Create), 2, Duration::ZERO, None, true), (100, None));
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
        assert_eq!(typical(OpKind::Resume), "about 30 seconds");

        // A plain resume: starting, then setting up, 12 and 18 seconds.
        let mut seen = [false, false, true, false];
        assert_eq!(shown_stages(OpKind::Resume, &seen, 2), [2, 3]);
        let (p, left) = progress(OpKind::Resume, &[2, 3], 2, Duration::from_secs(6), None, false);
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
        let (p, left) = progress(OpKind::Resume, &shown, 0, Duration::from_secs(30), three_of_eight, false);
        assert_eq!(p, 30);
        // 30 s for 3/8: 50 s more, and 30 s of the stages after.
        assert_eq!(left.as_deref(), Some("about 80 seconds"));
        // However long it has taken, done is done.
        let (p, _) = progress(OpKind::Resume, &shown, 0, Duration::from_secs(600), Some(StageProgress { done: 8, total: 8 }), false);
        assert_eq!(p, 80);
        // Without a report, time against the typical length, as before.
        let (p, _) = progress(OpKind::Resume, &shown, 0, Duration::from_secs(30), None, false);
        assert_eq!(p, 20);
        // A report with no total is no measure.
        let (p, _) = progress(OpKind::Resume, &shown, 0, Duration::from_secs(30), Some(StageProgress { done: 0, total: 0 }), false);
        assert_eq!(p, 20);
        // The copy is measured in bytes; a compaction only makes a share.
        assert_eq!(measure(OpKind::Resume, 0, StageProgress { done: 9, total: 8 }), StageMeasure { done: 8, total: 8, bytes: true });
        assert_eq!(measure(OpKind::Resume, 1, StageProgress { done: 40, total: 100 }), StageMeasure { done: 40, total: 100, bytes: false });
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
