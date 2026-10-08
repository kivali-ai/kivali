//! What the bundled pages, the tray and the menus show: the snapshot
//! (`src/types.ts` names the same keys) and the pure decisions behind
//! it: a team's state and phrase, an operation's stages and progress,
//! whether a team fits in memory.

use crate::supervisor::wire::{OrgState, Status};
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

    /// The stages of this kind of operation: the supervisor's stage id,
    /// the words people see, and a typical length (for the bar and "about
    /// N seconds").
    pub fn stages(self) -> &'static [(&'static str, &'static str, u64)] {
        match self {
            OpKind::Create => &[
                ("making-room", "Making room", 12),
                ("starting", "Starting up", 20),
                ("setting-up", "Setting up your team", 25),
                ("ready", "Ready", 0),
            ],
            OpKind::Resume => &[("starting", "Starting up", 12), ("setting-up", "Setting up your team", 18)],
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
}

/// Where an operation is: which stage, from the supervisor's stage id
/// (an id this kind does not know keeps the index it had), how long it
/// has spent in it.
pub fn stage_index(kind: OpKind, stage_id: Option<&str>) -> Option<usize> {
    let id = stage_id?;
    kind.stages().iter().position(|(s, _, _)| *s == id)
}

/// Rough share done and time left: the stages before the current one
/// count whole, the current one by time spent against its typical
/// length (never past 95% of it, so a slow stage does not read as done).
pub fn progress(kind: OpKind, index: usize, in_stage: Duration, finished: bool) -> (u8, Option<String>) {
    let stages = kind.stages();
    if finished {
        return (100, None);
    }
    let total: u64 = stages.iter().map(|s| s.2).sum::<u64>().max(1);
    let before: u64 = stages.iter().take(index).map(|s| s.2).sum();
    let typical = stages.get(index).map(|s| s.2).unwrap_or(0);
    let spent = (in_stage.as_secs_f64()).min(typical as f64 * 0.95);
    let done = before as f64 + spent;
    let percent = ((done / total as f64) * 100.0).clamp(0.0, 99.0) as u8;
    let left = (total as f64 - done).max(0.0);
    (percent, Some(about(left)))
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
    about(kind.stages().iter().map(|s| s.2).sum::<u64>() as f64)
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
        let (p, left) = progress(OpKind::Create, 1, Duration::from_secs(10), false);
        // 12 + 10 of 57 seconds.
        assert_eq!(p, 38);
        assert_eq!(left.as_deref(), Some("about 40 seconds"));
        // A slow stage never reads as past 95% of itself.
        let (p, _) = progress(OpKind::Pause, 0, Duration::from_secs(600), false);
        assert_eq!(p, 95);
        assert_eq!(progress(OpKind::Create, 2, Duration::ZERO, true), (100, None));
        assert_eq!(about(170.0), "about 3 minutes");
        assert_eq!(about(4.0), "a few seconds");
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
