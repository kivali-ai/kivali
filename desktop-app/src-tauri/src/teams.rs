//! `teams.json`: the shell's list of teams, in the shared config
//! directory.
//!
//! ```json
//! {"teams": [
//!    {"id": "plainsong-3f2a", "name": "Plainsong", "place": "here", "kind": "work",
//!     "dir": "teams/plainsong-3f2a", "port": 8080, "owner": "dana@example.com",
//!     "memory_mb": 4096, "cpus": 4},
//!    {"id": "9f3a0c12", "name": "Studio", "place": "elsewhere", "url": "https://dana-imac.local:8443"}],
//!  "start_at_login": true, "ask_before_quit": true,
//!  "running_at_quit": ["plainsong-3f2a"], "last_open": "plainsong-3f2a"}
//! ```
//!
//! A team on this computer has its own supervisor config directory
//! (`dir`, relative to the config directory): its own `local.json`, data
//! disk, socket and logs, always a folder under `teams/`.
//!
//! Reading is tolerant: each entry is parsed on its own and a bad one
//! costs only itself. A file that needed repair is copied to
//! `teams.json.bak` first; one that is not JSON is moved aside. Keys this
//! version does not know are kept.

use crate::orgurl;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};
use std::path::{Path, PathBuf};

pub const TEAMS_FILE: &str = "teams.json";
/// The directory new teams' supervisor folders live in.
pub const TEAMS_DIR: &str = "teams";

pub const DEFAULT_MEMORY_MB: u32 = 4096;
pub const DEFAULT_CPUS: u32 = 4;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Kind {
    Work,
    Personal,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Team {
    pub id: String,
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub kind: Option<Kind>,
    #[serde(flatten)]
    pub place: Place,
    /// RFC 3339; when it was last paused (by Pause or by Quit).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub paused_at: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "place", rename_all = "lowercase")]
pub enum Place {
    Here(Local),
    Elsewhere {
        url: String,
    },
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Local {
    /// The supervisor's config directory, relative to the config dir.
    pub dir: String,
    /// The port its forward last used; 0 before the first start.
    #[serde(default)]
    pub port: u16,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub owner: Option<String>,
    #[serde(default = "default_memory")]
    pub memory_mb: u32,
    #[serde(default = "default_cpus")]
    pub cpus: u32,
    /// The https address other computers reach the team at (Other devices). The
    /// operator provides it (Tailscale, a reverse proxy) in front of the
    /// team's loopback port; Kivali only checks it answers as this team.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub public_url: Option<String>,
    /// How the team's agents refer to the person (setup's "What should
    /// your agents call you?"), handed to the server at install.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub call_me: Option<String>,
    /// RFC 3339; when the team's Claude sign-in went from signed in to
    /// signed out (Settings' "Claude signed out on 2 Oct."), as the watch
    /// loop saw it. Cleared by the next sign-in.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub signed_out_at: Option<String>,
}

fn default_memory() -> u32 {
    DEFAULT_MEMORY_MB
}
fn default_cpus() -> u32 {
    DEFAULT_CPUS
}

impl Team {
    pub fn local(&self) -> Option<&Local> {
        match &self.place {
            Place::Here(l) => Some(l),
            Place::Elsewhere { .. } => None,
        }
    }
    pub fn local_mut(&mut self) -> Option<&mut Local> {
        match &mut self.place {
            Place::Here(l) => Some(l),
            Place::Elsewhere { .. } => None,
        }
    }
    pub fn is_here(&self) -> bool {
        matches!(self.place, Place::Here(_))
    }
    /// The supervisor directory of a team on this computer.
    pub fn dir(&self, config_dir: &Path) -> Option<PathBuf> {
        self.local().map(|l| config_dir.join(&l.dir))
    }
    /// The origin the team's window loads, when it has one: a team here
    /// once its port is known.
    pub fn origin(&self) -> Option<String> {
        match &self.place {
            Place::Here(l) if l.port != 0 => Some(format!("http://127.0.0.1:{}", l.port)),
            Place::Here(_) => None,
            Place::Elsewhere { url } => Some(url.trim_end_matches('/').to_string()),
        }
    }
    /// What to call the computer a team elsewhere runs on.
    pub fn device(&self) -> Option<String> {
        match &self.place {
            Place::Elsewhere { url } => url::Url::parse(url).ok().and_then(|u| u.host_str().map(str::to_string)),
            Place::Here(_) => None,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct TeamsFile {
    pub teams: Vec<Team>,
    #[serde(default)]
    pub start_at_login: bool,
    #[serde(default = "yes")]
    pub ask_before_quit: bool,
    /// Teams on this computer that were running when Kivali quit; they
    /// resume at the next launch.
    #[serde(default)]
    pub running_at_quit: Vec<String>,
    /// The team whose window was opened last.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_open: Option<String>,
    #[serde(flatten)]
    pub extra: Map<String, Value>,
}

fn yes() -> bool {
    true
}

impl Default for TeamsFile {
    fn default() -> Self {
        TeamsFile {
            teams: Vec::new(),
            start_at_login: false,
            ask_before_quit: true,
            running_at_quit: Vec::new(),
            last_open: None,
            extra: Map::new(),
        }
    }
}

/// What loading found, for the log.
#[derive(Debug, Default, PartialEq)]
pub struct LoadReport {
    pub repairs: Vec<String>,
    pub moved_aside: Option<PathBuf>,
}

/// A directory name and id for a new team: its name's letters and
/// digits, lower-cased, at most 8, then four random hex digits. Kept
/// short so the supervisor's socket path
/// (~/Library/Application Support/Kivali/teams/<id>/supervisor.sock)
/// stays under the 104-byte limit for a user name of up to 25
/// characters: 16 failed `bind` from 18 on (alexandra.williams).
pub fn new_id(name: &str, random: u16) -> String {
    let mut slug: String = name
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() { c.to_ascii_lowercase() } else { '-' })
        .collect();
    while slug.contains("--") {
        slug = slug.replace("--", "-");
    }
    let slug: String = slug.trim_matches('-').chars().take(8).collect();
    let slug = slug.trim_end_matches('-');
    if slug.is_empty() {
        format!("team-{random:04x}")
    } else {
        format!("{slug}-{random:04x}")
    }
}

impl TeamsFile {
    /// Parses and repairs: entries with no id, an unknown place, an
    /// unusable address or a repeated id are dropped; empty names get a
    /// default; addresses are reduced to their origin; ids in
    /// `running_at_quit` and `last_open` that name no team are dropped.
    pub fn parse(text: &str) -> Result<(TeamsFile, Vec<String>), serde_json::Error> {
        let mut top: Map<String, Value> = serde_json::from_str(text)?;
        let mut repairs = Vec::new();
        let raw = match top.remove("teams") {
            Some(Value::Array(a)) => a,
            Some(Value::Null) | None => Vec::new(),
            Some(_) => {
                repairs.push("`teams` was not a list; started an empty one".into());
                Vec::new()
            }
        };
        let mut flag = |key: &str, default: bool, top: &mut Map<String, Value>| match top.remove(key) {
            Some(Value::Bool(b)) => b,
            Some(Value::Null) | None => default,
            Some(_) => {
                repairs.push(format!("`{key}` was not true/false; set to {default}"));
                default
            }
        };
        let start_at_login = flag("start_at_login", false, &mut top);
        let ask_before_quit = flag("ask_before_quit", true, &mut top);
        let running_at_quit: Vec<String> = match top.remove("running_at_quit") {
            Some(Value::Array(a)) => a.into_iter().filter_map(|v| v.as_str().map(str::to_string)).collect(),
            _ => Vec::new(),
        };
        let last_open = top.remove("last_open").and_then(|v| v.as_str().map(str::to_string));

        let mut teams: Vec<Team> = Vec::new();
        for (i, raw) in raw.into_iter().enumerate() {
            let mut t: Team = match serde_json::from_value(raw) {
                Ok(t) => t,
                Err(e) => {
                    repairs.push(format!("dropped team #{i}: {e}"));
                    continue;
                }
            };
            t.id = t.id.trim().to_string();
            if t.id.is_empty() || t.id.contains(['/', '\\']) || t.id == "." || t.id == ".." {
                repairs.push(format!("dropped team #{i}: unusable id {:?}", t.id));
                continue;
            }
            if teams.iter().any(|o| o.id == t.id) {
                repairs.push(format!("dropped team #{i}: id {:?} is used twice", t.id));
                continue;
            }
            match &mut t.place {
                Place::Here(l) => {
                    if !is_team_dir(&l.dir) {
                        repairs.push(format!("dropped team #{i}: folder {:?} is not under {TEAMS_DIR}/", l.dir));
                        continue;
                    }
                    if teams.iter().any(|o| o.local().is_some_and(|ol| ol.dir == l.dir)) {
                        repairs.push(format!("dropped team #{i}: folder {:?} is used twice", l.dir));
                        continue;
                    }
                }
                Place::Elsewhere { url } => match orgurl::normalize_org_url(url) {
                    Ok(u) => {
                        let norm = orgurl::origin_string(&u);
                        if norm != *url {
                            repairs.push(format!("team {:?}: address {url:?} -> {norm:?}", t.id));
                            *url = norm;
                        }
                    }
                    Err(e) => {
                        repairs.push(format!("dropped team #{i}: {e}"));
                        continue;
                    }
                },
            }
            if t.name.trim().is_empty() {
                t.name = t.device().unwrap_or_else(|| "Team".into());
                repairs.push(format!("team {:?}: empty name -> {:?}", t.id, t.name));
            }
            teams.push(t);
        }
        let known = |id: &String| teams.iter().any(|t| &t.id == id && t.is_here());
        let running_at_quit = running_at_quit.into_iter().filter(known).collect();
        let last_open = last_open.filter(|id| teams.iter().any(|t| &t.id == id));
        Ok((TeamsFile { teams, start_at_login, ask_before_quit, running_at_quit, last_open, extra: top }, repairs))
    }

    pub fn to_json(&self) -> String {
        let mut s = serde_json::to_string_pretty(self).expect("teams.json serialises");
        s.push('\n');
        s
    }

    /// Reads `dir/teams.json`; when there is none, no teams yet.
    pub fn load(dir: &Path, now_unix: u64) -> std::io::Result<(TeamsFile, LoadReport)> {
        let path = dir.join(TEAMS_FILE);
        let text = match std::fs::read_to_string(&path) {
            Ok(t) => t,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok((TeamsFile::default(), LoadReport::default())),
            Err(e) => return Err(e),
        };
        match TeamsFile::parse(&text) {
            Ok((file, repairs)) => {
                if !repairs.is_empty() {
                    let bak = dir.join(format!("{TEAMS_FILE}.bak"));
                    crate::paths::write_atomic(&bak, text.as_bytes(), 0o600)?;
                }
                Ok((file, LoadReport { repairs, ..Default::default() }))
            }
            Err(e) => {
                let aside = dir.join(format!("{TEAMS_FILE}.unreadable-{now_unix}"));
                std::fs::rename(&path, &aside)?;
                Ok((
                    TeamsFile::default(),
                    LoadReport {
                        repairs: vec![format!("teams.json was not readable ({e}); moved aside")],
                        moved_aside: Some(aside),
                    },
                ))
            }
        }
    }

    pub fn save(&self, dir: &Path) -> std::io::Result<()> {
        crate::paths::write_atomic(&dir.join(TEAMS_FILE), self.to_json().as_bytes(), 0o600)
    }

    pub fn get(&self, id: &str) -> Option<&Team> {
        self.teams.iter().find(|t| t.id == id)
    }

    pub fn get_mut(&mut self, id: &str) -> Option<&mut Team> {
        self.teams.iter_mut().find(|t| t.id == id)
    }

    /// Adds a team on this computer with a fresh id and folder.
    pub fn add_here(&mut self, name: &str, kind: Kind, owner: &str, random: &mut dyn FnMut() -> u16) -> String {
        let mut id = new_id(name, random());
        while self.get(&id).is_some() {
            id = new_id(name, random());
        }
        self.add_here_as(&id, name, kind, owner);
        id
    }

    /// Adds a team on this computer under `id` (its folder `teams/<id>`),
    /// for a machine prepared before setup knew the team's name.
    pub fn add_here_as(&mut self, id: &str, name: &str, kind: Kind, owner: &str) {
        let id = id.to_string();
        self.teams.push(Team {
            id: id.clone(),
            name: name.trim().to_string(),
            kind: Some(kind),
            place: Place::Here(Local {
                dir: format!("{TEAMS_DIR}/{id}"),
                port: 0,
                owner: Some(owner.to_string()),
                memory_mb: DEFAULT_MEMORY_MB,
                cpus: DEFAULT_CPUS,
                public_url: None,
                call_me: None,
                signed_out_at: None,
            }),
            paused_at: None,
        });
    }

    /// Adds a team elsewhere, or returns the id of the one already at
    /// that origin. `origin` comes from [`orgurl::normalize_org_url`].
    pub fn add_elsewhere(&mut self, origin: &url::Url, name: &str) -> String {
        let url = orgurl::origin_string(origin);
        if let Some(t) = self.teams.iter().find(|t| matches!(&t.place, Place::Elsewhere { url: u } if *u == url)) {
            return t.id.clone();
        }
        let base = format!("{:08x}", fnv1a(url.as_bytes()));
        let mut id = base.clone();
        let mut n = 2;
        while self.get(&id).is_some() {
            id = format!("{base}-{n}");
            n += 1;
        }
        let host = origin.host_str().unwrap_or_default().to_string();
        let name = if name.trim().is_empty() { host } else { name.trim().to_string() };
        self.teams.push(Team { id: id.clone(), name, kind: None, place: Place::Elsewhere { url }, paused_at: None });
        id
    }

    pub fn rename(&mut self, id: &str, name: &str) -> bool {
        let name = name.trim();
        match self.get_mut(id) {
            Some(t) if !name.is_empty() && t.name != name => {
                t.name = name.to_string();
                true
            }
            _ => false,
        }
    }

    pub fn remove(&mut self, id: &str) -> Option<Team> {
        let i = self.teams.iter().position(|t| t.id == id)?;
        self.running_at_quit.retain(|r| r != id);
        if self.last_open.as_deref() == Some(id) {
            self.last_open = None;
        }
        Some(self.teams.remove(i))
    }

    /// Puts the teams in the order of `ids`; unknown ids are ignored and
    /// teams not named keep their relative order at the end.
    pub fn reorder(&mut self, ids: &[String]) {
        let mut rest = std::mem::take(&mut self.teams);
        for id in ids {
            if let Some(i) = rest.iter().position(|t| &t.id == id) {
                self.teams.push(rest.remove(i));
            }
        }
        self.teams.append(&mut rest);
    }
}

/// `teams/<id>` with one path segment that is not `.` or `..`.
fn is_team_dir(d: &str) -> bool {
    match d.strip_prefix(TEAMS_DIR).and_then(|r| r.strip_prefix('/')) {
        Some(seg) => !seg.is_empty() && !seg.contains(['/', '\\']) && seg != "." && seg != "..",
        None => false,
    }
}

fn fnv1a(bytes: &[u8]) -> u32 {
    let mut h: u32 = 0x811c9dc5;
    for b in bytes {
        h ^= u32::from(*b);
        h = h.wrapping_mul(0x01000193);
    }
    h
}

#[cfg(test)]
mod tests {
    use super::*;

    const EXAMPLE: &str = r#"{
      "teams": [
        {"id": "plainsong-3f2a", "name": "Plainsong", "kind": "work", "place": "here",
         "dir": "teams/plainsong-3f2a", "port": 8080, "owner": "dana@example.com",
         "memory_mb": 4096, "cpus": 4},
        {"id": "9f3a0c12", "name": "Studio", "place": "elsewhere", "url": "https://dana-imac.local:8443"}
      ],
      "start_at_login": true,
      "ask_before_quit": true,
      "running_at_quit": ["plainsong-3f2a"],
      "last_open": "plainsong-3f2a"
    }"#;

    #[test]
    fn reads_and_round_trips_the_example() {
        let (f, repairs) = TeamsFile::parse(EXAMPLE).unwrap();
        assert!(repairs.is_empty(), "{repairs:?}");
        assert_eq!(f.teams.len(), 2);
        assert_eq!(f.teams[0].origin().as_deref(), Some("http://127.0.0.1:8080"));
        assert_eq!(f.teams[1].device().as_deref(), Some("dana-imac.local"));
        assert_eq!(f.teams[0].dir(Path::new("/c")), Some(PathBuf::from("/c/teams/plainsong-3f2a")));
        let v: Value = serde_json::from_str(&f.to_json()).unwrap();
        let want: Value = serde_json::from_str(EXAMPLE).unwrap();
        assert_eq!(v, want);
    }

    #[test]
    fn repairs_one_entry_at_a_time() {
        let text = r#"{"teams": [
            {"id": "a", "name": "A", "place": "here", "dir": "teams/a"},
            {"id": "a", "name": "A again", "place": "here", "dir": "teams/a2"},
            {"id": "b", "name": "B", "place": "here", "dir": "../escape"},
            {"id": "c", "name": "C", "place": "here", "dir": "teams/a"},
            {"id": "d/e", "name": "D", "place": "here", "dir": "teams/d"},
            {"id": "f", "name": "", "place": "elsewhere", "url": "f.example.com/x"},
            {"id": "g", "name": "G", "place": "elsewhere", "url": "http://g.example.com"},
            {"id": "h", "name": "H", "place": "moon"}
          ],
          "running_at_quit": ["a", "f", "zz"], "last_open": "zz", "ask_before_quit": "no"}"#;
        let (f, repairs) = TeamsFile::parse(text).unwrap();
        let ids: Vec<&str> = f.teams.iter().map(|t| t.id.as_str()).collect();
        assert_eq!(ids, ["a", "f"]);
        assert_eq!(f.teams[1].name, "f.example.com");
        assert_eq!(f.running_at_quit, ["a"]);
        assert_eq!(f.last_open, None);
        assert!(f.ask_before_quit);
        assert_eq!(f.teams[0].local().unwrap().memory_mb, DEFAULT_MEMORY_MB);
        assert!(repairs.len() >= 8, "{repairs:#?}");
    }

    #[test]
    fn ids_are_short_slugs() {
        assert_eq!(new_id("Plainsong", 0x3f2a), "plainson-3f2a");
        assert_eq!(new_id("  The Garden & Home!! ", 1), "the-gard-0001");
        assert_eq!(new_id("A very long team name indeed", 0xffff), "a-very-l-ffff");
        assert_eq!(new_id("Ab", 3), "ab-0003");
        assert_eq!(new_id("日本", 2), "team-0002");
    }

    // ~/Library/Application Support/Kivali/teams/<id>/supervisor.sock is
    // 65 bytes plus the user name plus the id; a socket path holds 103.
    // The longest id leaves room for a 25-character user name.
    #[test]
    fn ids_leave_the_socket_path_room() {
        let longest = new_id("abcdefghijklmnopqrstuvwxyz", 0xffff);
        assert_eq!(longest.len(), 13, "{longest}");
        let path = format!("/Users/{}/Library/Application Support/Kivali/teams/{longest}/supervisor.sock", "a".repeat(25));
        assert!(path.len() <= 103, "{} bytes: {path}", path.len());
    }

    #[test]
    fn add_remove_reorder() {
        let mut f = TeamsFile::default();
        let mut n = 0u16;
        let mut rnd = || {
            n += 1;
            n
        };
        let a = f.add_here("Plainsong", Kind::Work, "dana@example.com", &mut rnd);
        let b = f.add_here("Home", Kind::Personal, "dana@example.com", &mut rnd);
        let o = orgurl::normalize_org_url("studio.example.com").unwrap();
        let c = f.add_elsewhere(&o, "Studio");
        assert_eq!(f.add_elsewhere(&o, "Again"), c);
        assert_eq!(f.get(&a).unwrap().local().unwrap().dir, format!("teams/{a}"));
        f.reorder(&[c.clone(), a.clone()]);
        let ids: Vec<&str> = f.teams.iter().map(|t| t.id.as_str()).collect();
        assert_eq!(ids, [c.as_str(), a.as_str(), b.as_str()]);
        f.running_at_quit = vec![a.clone()];
        f.last_open = Some(a.clone());
        assert!(f.remove(&a).is_some());
        assert!(f.running_at_quit.is_empty() && f.last_open.is_none());
    }

    #[test]
    fn unreadable_file_is_moved_aside() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join(TEAMS_FILE), "{ nope").unwrap();
        let (f, report) = TeamsFile::load(dir.path(), 7).unwrap();
        assert_eq!(f, TeamsFile::default());
        assert_eq!(report.moved_aside, Some(dir.path().join("teams.json.unreadable-7")));
    }
}
