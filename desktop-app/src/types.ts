// What the shell hands the bundled pages (commands.rs, `shell_snapshot`),
// and the shapes its commands take and return. The Rust side is
// src-tauri/src/shell.rs; a test there serializes a snapshot and checks
// the keys this file names.

/** A team's state everywhere: tray, Settings, its window. */
export type TeamState = "running" | "starting" | "paused" | "failed";

/** What a "starting" team is doing, for the words on its page. */
export type Activity = "creating" | "waking" | "pausing" | "updating" | "applying" | "deleting";

export type Kind = "work" | "personal";

/** Where a team runs: on this computer, or reached by its address. */
export type Place = "here" | "elsewhere";

export type OpKind = "create" | "resume" | "pause" | "update" | "address" | "delete";

/** The team's current or last long operation. */
export interface OpView {
  kind: OpKind;
  running: boolean;
  /** Set when it ended in error. */
  error: string | null;
  finished: boolean;
  /** The supervisor's raw lines, oldest first ("Show details"). */
  lines: string[];
  /** The stage in people's words ("Starting up"), its place, and the
   *  stages of this kind of operation in order. */
  stage: string | null;
  stage_index: number;
  stages: string[];
  /** Unix milliseconds. */
  started_ms: number;
  /** Rough share done, 0–100, from the stage and the time spent in it. */
  percent: number;
  /** "about 40 seconds", or null when the shell has no estimate. */
  remaining: string | null;
}

export interface AiView {
  provider: "claude";
  /** Whether Claude Code is signed in (`claude auth status`); null when the shell could not ask. */
  signed_in: boolean | null;
  /** The account the CLI is signed in to, when it reports one. */
  email: string | null;
  /** What model calls are billed to, in the CLI's terms: "Claude Max",
   *  "Anthropic Console", "Amazon Bedrock", "Google Vertex AI", or after
   *  a sign-in setup "<provider> · <target>". */
  billing: string | null;
  /** A Terminal running Claude's sign-in for this team is open. */
  terminal_open: boolean;
  /** Claude's sign-in in Terminal: not opened (or done), opened and not
   *  finished yet, or Terminal closed before a credential appeared. */
  signin: "idle" | "waiting" | "closed";
  /** RFC 3339: when Claude signed out, while it is signed out. */
  signed_out_at: string | null;
}

/** `credential_setup`: the model ids a team runs the CLI with, and the
 *  sign-in setup saved now (its values without secrets). */
export interface SetupInfo {
  models: string[];
  current?: { setup: string; values: Record<string, string> };
}

/** One model's answer after a sign-in setup: `missing` when the provider
 *  has no model by that name; `problem` says why it didn't answer. */
export interface ModelCheck {
  model: string;
  ok: boolean;
  missing?: boolean;
  problem?: string;
}

/** `apply_credential_setup`'s answer: the sign-in Claude reports now, and each model's check. */
export interface SetupResult {
  credential: { signed_in: boolean; email?: string; billing?: string; checked_at: string };
  models: ModelCheck[];
}

export type TeamUpdateState =
  | "not_checked"
  | "up_to_date"
  | "available"
  | "desktop_first"
  | "manual_steps"
  | "failed";

export interface TeamUpdate {
  state: TeamUpdateState;
  current: string | null;
  latest: string | null;
  notes_url: string | null;
  /** release.json's one-line summary, when it has one. */
  summary: string | null;
  checked_at: string | null;
  /** How long an update typically takes: "about 3 minutes". */
  takes: string;
}

export interface Team {
  id: string;
  name: string;
  kind: Kind | null;
  place: Place;
  /** The team's origin, when known (a team on this Mac has one while running). */
  url: string | null;
  /** For a team elsewhere: what to call the computer ("dana-imac.local"). */
  device: string | null;
  state: TeamState;
  activity: Activity | null;
  /** The short phrase beside the dot: "running", "paused", "updating",
   *  "couldn't start", "Claude isn't signed in", "on dana-imac.local". */
  phrase: string;
  /** Why it couldn't start, in words, when the shell knows. */
  reason: string | null;
  /** Set when the reason is memory: the running team to pause first. */
  pause_first: string | null;
  op: OpView | null;
  version: string | null;
  update: TeamUpdate | null;
  ai: AiView | null;
  owner: string | null;
  memory_mb: number;
  cpus: number;
  disk_used_bytes: number | null;
  disk_size_bytes: number | null;
  /** Other devices: the https address the operator set up in front of
   *  the team (Tailscale, a reverse proxy), checked by `set_public_url`. */
  public_url: string | null;
  /** RFC 3339. */
  paused_since: string | null;
  last_reached: string | null;
  /** Created in this run of the app and never ready yet (setup can abandon it). */
  provisional: boolean;
  /** From the team's own API, as the person signed in in its window
   *  (src-tauri/src/teamapi.rs); null until read. Agents exclude the
   *  person's own seat; working only while the team runs; files are the
   *  team's Files page. */
  agents: number | null;
  working: number | null;
  files: number | null;
  /** The Google account the team's window is signed in with. */
  signed_in_as: string | null;
  /** A team on this computer: the memory the shell would let it use beside
   *  the teams running now (the memory dialog's "this Mac has 2 GB free"); null elsewhere. */
  memory_free_mb: number | null;
}

export interface OwnerSignin {
  state: "idle" | "waiting" | "signed_in" | "failed";
  email: string | null;
  error: string | null;
}

export interface ConnectState {
  state: "idle" | "signing_in" | "not_invited" | "done" | "failed";
  origin: string | null;
  name: string | null;
  error: string | null;
  /** The team added, once done. */
  team_id: string | null;
  /** The Google account the team refused ("sam@example.com can’t sign in"),
   *  from the team's `kivali_denied` cookie; null when unknown. */
  email: string | null;
}

export interface AppUpdate {
  state: "not_checked" | "checking" | "up_to_date" | "available" | "installing" | "failed";
  version: string | null;
  checked_at: string | null;
  error: string | null;
}

export interface Snapshot {
  app_version: string;
  platform: "macos" | "windows" | "linux";
  teams: Team[];
  host: { memory_mb: number; cpus: number };
  settings: { start_at_login: boolean; ask_before_quit: boolean };
  app_update: AppUpdate;
  owner_signin: OwnerSignin;
  connect: ConnectState;
  config_dir: string;
  logs_dir: string;
  /** The last team deleted in this run, for D11. */
  last_deleted: { name: string; freed_bytes: number } | null;
}

/** `connect_check`'s answer. */
export interface ConnectFound {
  origin: string;
  name: string;
  host: string;
}

/** `connect_check`'s error, as the page tells them apart. */
export interface ConnectError {
  kind: "unreachable" | "not_kivali" | "invalid";
  message: string;
}

/** `create_team`'s argument. */
export interface NewTeam {
  name: string;
  kind: Kind;
  owner: string;
  /** How the team's agents refer to the person ("Jane", "Mom", "CEO"); may be empty. */
  call_me: string;
}

