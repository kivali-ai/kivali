// Decisions about a team: its dot and words, the Kivali page its
// window shows when the team can't show itself, Settings rows and the
// dialogs' words. Pure (teams.test.ts).

import type { Snapshot, Team, TeamState } from "../types";
import { ago, deviceName, formatBytes, formatMemory, shortDate, shortVersion, sinceWhen } from "./format";
import { clampPercent } from "./setup";

/** The dot's class and the word beside it. */
export function dotOf(state: TeamState): { cls: string; word: string } {
  switch (state) {
    case "running":
      return { cls: "dot--running", word: "Running" };
    case "starting":
      return { cls: "dot--starting", word: "Starting" };
    case "paused":
      return { cls: "dot--paused", word: "Paused" };
    case "failed":
      return { cls: "dot--failed", word: "Couldn’t start" };
  }
}

/** "Work · on this Mac", "Personal · on Dana’s iMac", "On this Mac". */
export function placeLine(team: Team, platform: Snapshot["platform"]): string {
  const where = team.place === "here" ? `on ${thisComputer(platform)}` : `on ${deviceName(team.device)}`;
  const kind = team.kind === "work" ? "Work" : team.kind === "personal" ? "Personal" : null;
  return kind ? `${kind} · ${where}` : where[0].toUpperCase() + where.slice(1);
}

/** "this Mac" on macOS, "this PC" on Windows, "this computer" elsewhere. */
export function thisComputer(platform: Snapshot["platform"]): string {
  return platform === "macos" ? "this Mac" : platform === "windows" ? "this PC" : "this computer";
}

export type TeamPage =
  | { kind: "paused"; title: string; body: string; action: string; since: string | null }
  | { kind: "progress"; title: string; body: string | null; percent: number; step: string | null }
  | {
      kind: "failed";
      title: string;
      body: string;
      action: { label: string; pause: string | null };
      details: string | null;
    }
  | { kind: "unreachable"; title: string; body: string; last: string | null }
  | { kind: "gone"; title: string; body: string };

function sentence(s: string): string {
  const t = s.trim();
  if (!t) return t;
  const cap = t[0].toUpperCase() + t.slice(1);
  return /[.?!…]$/.test(cap) ? cap : `${cap}.`;
}

/** The Kivali page shown in a team's window when the team can't show itself. */
export function teamPage(team: Team | undefined, snap: Pick<Snapshot, "teams" | "platform">, now: Date): TeamPage {
  if (!team) return { kind: "gone", title: "This team isn’t here anymore", body: "It was deleted or removed from this computer." };
  const name = team.name;
  if (team.place === "elsewhere") {
    if (team.state === "running") return { kind: "progress", title: `Opening ${name}`, body: null, percent: 100, step: null };
    return {
      kind: "unreachable",
      title: `Can’t reach ${deviceName(team.device)}`,
      body: `${name} runs there. It may be asleep or offline.`,
      last: team.last_reached ? `Last reached ${ago(team.last_reached, now)}` : null,
    };
  }
  const op = team.op;
  if (team.state === "failed") {
    const other = team.pause_first ? snap.teams.find((t) => t.id === team.pause_first) : undefined;
    const updateFailed = op?.kind === "update";
    let body: string;
    if (team.reason) body = sentence(team.reason);
    else if (other) body = `This Mac is low on memory. ${other.name} is using ${formatMemory(other.memory_mb)}.`;
    else if (updateFailed && team.version) body = `The update didn’t finish, so ${name} went back to ${shortVersion(team.version)}.`;
    else body = "Something went wrong while starting.";
    const title = updateFailed && !other ? `${name} couldn’t update` : `${name} couldn’t start`;
    const details = op && (op.lines.length || op.error) ? [...op.lines, ...(op.error ? [op.error] : [])].join("\n") : null;
    return {
      kind: "failed",
      title,
      body,
      action: other ? { label: `Pause ${other.name} and try again`, pause: other.id } : { label: "Try again", pause: null },
      details,
    };
  }
  if (team.state === "paused") {
    const since = team.paused_since ? sinceWhen(team.paused_since, now) : null;
    return {
      kind: "paused",
      title: `${name} is paused`,
      body: "Your agents aren’t working. Nothing is lost.",
      action: `Resume ${name}`,
      since: since ? `Paused since ${since}` : null,
    };
  }
  const remaining = op?.running && op.remaining ? sentence(op.remaining) : null;
  const percent = op?.running ? clampPercent(op.percent) : 0;
  switch (team.activity) {
    case "updating": {
      const to = shortVersion(team.update?.latest ?? null);
      const step =
        op?.running && op.stage && op.stages.length
          ? `${op.stage} · step ${Math.min(op.stage_index + 1, op.stages.length)} of ${op.stages.length}`
          : (op?.stage ?? null);
      return {
        kind: "progress",
        title: to ? `Updating ${name} to ${to}` : `Updating ${name}`,
        body: [remaining, "Agents pick up where they left off."].filter(Boolean).join(" "),
        percent,
        step,
      };
    }
    case "pausing":
      return {
        kind: "progress",
        title: `Pausing ${name}`,
        body: [remaining, "Agents stop and pick up where they left off when you resume."].filter(Boolean).join(" "),
        percent,
        step: null,
      };
    case "creating":
      return { kind: "progress", title: `Getting ${name} ready`, body: remaining, percent, step: null };
    case "applying":
      return { kind: "progress", title: `Applying changes to ${name}`, body: remaining, percent, step: null };
    case "deleting":
      return { kind: "progress", title: `Deleting ${name}`, body: remaining, percent, step: null };
    default:
      return { kind: "progress", title: `Waking ${name} up`, body: remaining, percent, step: null };
  }
}

/** "3 agents working", "1 agent working"; null when none are or it isn't known
 *  (the shell's phrase says the same: view.rs `phrase`). */
export function agentsWorking(working: number | null | undefined): string | null {
  if (working == null || working <= 0) return null;
  return working === 1 ? "1 agent working" : `${working.toLocaleString("en-US")} agents working`;
}

/** The update dialog's sentence: "It takes about 3 minutes. Agents pause while it updates. If
 *  anything goes wrong, Plainsong goes back to 0.16." */
export function updateText(team: Team): string {
  const from = shortVersion(team.update?.current ?? team.version);
  const back = from ? ` If anything goes wrong, ${team.name} goes back to ${from}.` : "";
  return `It takes ${team.update?.takes ?? "a few minutes"}. Agents pause while it updates.${back}`;
}

/** The AI tab's signed-out banner line: "Claude signed out on 2 Oct." */
export function signedOutLine(team: Team, now: Date): string | null {
  const at = team.ai?.signed_out_at;
  if (!at) return null;
  const d = new Date(at);
  return Number.isNaN(d.getTime()) ? null : `Claude signed out on ${shortDate(d, now)}.`;
}

/** How Claude is signed in, as the CLI reports it: "dana@example.com · Claude Max",
 *  "Amazon Bedrock", or "Signed in" when it names neither. */
export function claudeLine(team: Team): string {
  const ai = team.ai;
  const parts = [ai?.email, ai?.billing].filter((p): p is string => !!p);
  return parts.length ? parts.join(" · ") : "Signed in";
}

/** The Overview tab's status row: the word, the line under it, and its button. */
export function statusRow(
  team: Team,
  snap: Pick<Snapshot, "teams">,
  now: Date,
): { cls: string; word: string; sub: string | null; action: "pause" | "resume" | "retry" | null; actionLabel: string | null } {
  const { cls, word } = dotOf(team.state);
  switch (team.state) {
    case "running":
      return {
        cls,
        word,
        sub: team.ai?.signed_in === false ? "Claude isn’t signed in" : (agentsWorking(team.working) ?? "Agents can work"),
        action: "pause",
        actionLabel: "Pause…",
      };
    case "paused": {
      const since = team.paused_since ? sinceWhen(team.paused_since, now) : null;
      return { cls, word, sub: since ? `Since ${since}` : null, action: "resume", actionLabel: "Resume" };
    }
    case "failed": {
      const other = team.pause_first ? snap.teams.find((t) => t.id === team.pause_first) : undefined;
      return {
        cls,
        word,
        sub: team.reason ?? (other ? `This Mac is low on memory. ${other.name} is using ${formatMemory(other.memory_mb)}.` : null),
        action: "retry",
        actionLabel: other ? `Pause ${other.name} and try again` : "Try again",
      };
    }
    case "starting": {
      const words: Record<string, string> = {
        creating: "Getting ready",
        waking: "Waking up",
        pausing: "Pausing",
        updating: "Updating",
        applying: "Applying changes",
        deleting: "Deleting",
      };
      const op = team.op;
      const sub = op?.running ? [op.stage, op.remaining].filter(Boolean).join(" · ") || null : null;
      return { cls, word: words[team.activity ?? "waking"] ?? "Starting", sub, action: null, actionLabel: null };
    }
  }
}

/** The Overview tab's update row, or null when there's no version to talk about. */
export function updateRow(
  team: Team,
  now: Date,
): { title: string; sub: string | null; highlight: boolean; update: boolean; notes: string | null } | null {
  const u = team.update;
  const current = (u?.current ?? team.version)?.replace(/^v/, "") ?? null;
  if (!current && !u) return null;
  const title = current ? `Kivali ${current}` : "Kivali";
  const latest = u?.latest?.replace(/^v/, "") ?? null;
  const checked = u?.checked_at ? ago(u.checked_at, now) : null;
  switch (u?.state) {
    case "available":
      return {
        title,
        sub: [`${latest} is available`, u.summary].filter(Boolean).join(" · "),
        highlight: true,
        update: true,
        notes: u.notes_url,
      };
    case "desktop_first":
      return { title, sub: `${latest} is available · update this app first, in General`, highlight: true, update: false, notes: u.notes_url };
    case "manual_steps":
      return { title, sub: `${latest} is available · it needs a few steps by hand`, highlight: true, update: false, notes: u.notes_url };
    case "up_to_date":
      return { title, sub: checked ? `Up to date · checked ${checked}` : "Up to date", highlight: false, update: false, notes: null };
    case "failed":
      return { title, sub: "Couldn’t check for updates", highlight: false, update: false, notes: null };
    default:
      return { title, sub: null, highlight: false, update: false, notes: null };
  }
}

/** This Mac tab: the memory bar's segments and its label. */
export function memoryUse(
  team: Team,
  snap: Pick<Snapshot, "teams" | "host" | "platform">,
): { label: string; segments: { name: string; mb: number; pct: number; self: boolean }[]; freeMb: number } {
  const total = Math.max(1, snap.host.memory_mb);
  const running = snap.teams.filter((t) => t.place === "here" && (t.state === "running" || t.state === "starting"));
  const used = running.reduce((n, t) => n + t.memory_mb, 0);
  const ordered = [...running].sort((a, b) => (a.id === team.id ? -1 : b.id === team.id ? 1 : 0));
  const segments = ordered.map((t) => ({
    name: t.name,
    mb: t.memory_mb,
    pct: Math.min(100, (t.memory_mb / total) * 100),
    self: t.id === team.id,
  }));
  const n = running.length;
  const label = `Memory on ${thisComputer(snap.platform)} · ${n === 1 ? "1 team" : `${n} teams`} running · ${trimGb(used)} of ${formatMemory(total)}`;
  return { label, segments, freeMb: Math.max(0, total - used) };
}

function trimGb(mb: number): string {
  return formatMemory(mb).replace(/ GB$/, "");
}

/** This Mac tab's select options: the usual sizes that fit, plus the current value. */
export function memoryOptions(hostMb: number, current: number): { value: number; label: string }[] {
  const cap = Math.max(2048, hostMb / 2);
  const base = [2048, 4096, 6144, 8192, 12288, 16384].filter((v) => v >= 4096 || hostMb <= 8192).filter((v) => v <= cap);
  return withCurrent(base, current).map((v) => ({ value: v, label: formatMemory(v) }));
}

export function cpuOptions(hostCpus: number, current: number): { value: number; label: string }[] {
  const base = [2, 4, 6, 8, 12].filter((v) => v <= Math.max(2, hostCpus));
  return withCurrent(base, current).map((v) => ({ value: v, label: String(v) }));
}

function withCurrent(base: number[], current: number): number[] {
  const set = new Set(base);
  if (current > 0) set.add(current);
  return [...set].sort((a, b) => a - b);
}

/** This Mac tab's disk card. */
export function diskLines(team: Team): { title: string; sub: string } {
  const used = team.disk_used_bytes;
  const size = team.disk_size_bytes;
  const tail = "Only what’s used takes space.";
  return {
    title: used != null ? `${formatBytes(used)} used` : "Not measured yet",
    sub: size != null ? `Of ${formatBytes(size)} reserved for ${team.name}. ${tail}` : tail,
  };
}

/** The delete dialog's list, with the numbers the shell knows. */
export function deleteList(
  team: Team,
  facts: { agents: number | null; files: number | null; disk_used_bytes: number | null } | null,
  platform: Snapshot["platform"],
): string[] {
  // Fresh from team_facts, else what the snapshot last had.
  const agents = facts?.agents ?? team.agents;
  const files = facts?.files ?? team.files;
  const disk = facts?.disk_used_bytes ?? team.disk_used_bytes;
  const fmt = (n: number) => n.toLocaleString("en-US");
  return [
    agents == null ? "Its agents and what they remember" : `${fmt(agents)} ${agents === 1 ? "agent" : "agents"} and what they remember`,
    files == null ? "Its files" : `${fmt(files)} ${files === 1 ? "file" : "files"}`,
    "Every chat and assignment",
    "The Claude sign-in",
    disk == null ? `Its disk on ${thisComputer(platform)}` : `${formatBytes(disk)} on ${thisComputer(platform)}`,
  ];
}

/** The delete confirmation's line about the OS prompt that follows. */
export function osPromptLine(platform: Snapshot["platform"]): string {
  if (platform === "macos") return "Next, your Mac asks for your password or Touch ID.";
  if (platform === "windows") return "Next, Windows asks you to confirm it’s you.";
  return "Next, your computer asks for your password.";
}

/** resume_team throws "memory:<id>" when the team won't fit. */
export function memoryConflict(err: unknown): string | null {
  if (typeof err !== "string") return null;
  const m = /^memory:(.+)$/.exec(err.trim());
  return m ? m[1] : null;
}

/** The memory dialog's words. */
export function memoryDialog(team: Team, other: Team | undefined, platform: Snapshot["platform"]) {
  const otherName = other?.name ?? "another team";
  // The shell's own estimate (view.rs `memory_free_mb`), as the native memory alert says it.
  const free = team.memory_free_mb;
  const has = free == null ? "doesn’t have that free" : `has ${formatMemory(free)} free`;
  return {
    title: `Not enough memory for ${team.name}`,
    body: `${team.name} needs ${formatMemory(team.memory_mb)}, and ${thisComputer(platform)} ${has} while ${otherName} runs. Pause ${otherName} to resume ${team.name}?`,
    confirm: `Pause ${otherName} and resume`,
  };
}

/** Polling: the shell emits shell-changed, but a running op's percent moves without it. */
export function shouldPoll(snap: Snapshot): boolean {
  if (snap.owner_signin.state === "waiting") return true;
  if (snap.connect.state === "signing_in") return true;
  if (snap.app_update.state === "checking" || snap.app_update.state === "installing") return true;
  return snap.teams.some((t) => t.op?.running || t.ai?.signin === "waiting" || t.state === "starting");
}

/** The page's own memory of the Other devices tab. */
export interface DevicesLocal {
  /** The switch was turned on here, before an address is saved. */
  revealed: boolean;
  /** "Change" reopened the field over a saved address. */
  editing: boolean;
}

/** Which state shows: off, the address field, or the saved address. */
export function devicesView(team: Pick<Team, "public_url">, local: DevicesLocal): "off" | "form" | "on" {
  if (team.public_url) return local.editing ? "form" : "on";
  return local.revealed ? "form" : "off";
}

/** The field's hint, in three parts so the command can be set in mono. */
export function devicesHint(team: Pick<Team, "url">): [string, string, string] {
  return [
    "Kivali doesn’t set this up. Put this Mac’s team behind https yourself, for example with ",
    "tailscale serve",
    `, pointing at ${team.url ?? "its local address"}.`,
  ];
}

/** "Who can sign in": the team's owner. */
export function signInLine(team: Pick<Team, "owner">): string {
  return team.owner ?? "The team's owner";
}
