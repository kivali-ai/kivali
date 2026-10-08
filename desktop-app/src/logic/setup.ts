// The create path's decisions: which screen shows for the page's
// answers and the shell's snapshot, the getting-ready bar, and when Back
// abandons the team.

import type { ConnectError, Kind, OpView, Snapshot, Team } from "../types";

export type Step = 1 | 2 | 3 | 4;

/** What the page remembers between snapshots. */
export interface SetupState {
  step: Step;
  kind: Kind | null;
  name: string;
  /** How the team's agents refer to the person ("Jane", "Mom", "CEO"); optional. */
  callMe: string;
  /** Set once create_team returned, with the answers it was given. */
  teamId: string | null;
  created: { name: string; kind: Kind; callMe: string } | null;
  /** The getting-ready bar shows its raw lines. */
  barOpen: boolean;
  /** The create-failed screen shows every line. */
  detailsOpen: boolean;
  busy: boolean;
}

export function newSetup(): SetupState {
  return {
    step: 1,
    kind: null,
    name: "",
    callMe: "",
    teamId: null,
    created: null,
    barOpen: false,
    detailsOpen: false,
    busy: false,
  };
}

export type SetupScreen =
  | "kind" | "kind-work" | "kind-personal"
  | "owner" | "owner-waiting" | "owner-signed-in" | "owner-failed"
  | "account-preparing" | "account-ready" | "account-waiting" | "account-signed-in" | "account-closed"
  | "done-work" | "done-personal"
  | "create-failed";

/** Ready: running, and the create operation (if any) done without error. */
export function teamReady(team: Team | undefined | null): boolean {
  if (!team) return false;
  if (team.state !== "running") return false;
  return !team.op || !team.op.running;
}

export function createFailed(team: Team | undefined | null): boolean {
  return !!team?.op && team.op.kind === "create" && !team.op.running && !!team.op.error;
}

export function setupScreen(s: SetupState, snap: Pick<Snapshot, "teams" | "owner_signin">): SetupScreen {
  const team = s.teamId ? snap.teams.find((t) => t.id === s.teamId) : undefined;
  if (team && createFailed(team)) return "create-failed";
  const kindScreen = s.kind === "personal" ? "done-personal" : "done-work";
  switch (s.step) {
    case 1:
      return s.kind === null ? "kind" : s.kind === "work" ? "kind-work" : "kind-personal";
    case 2:
      return ownerScreen(snap);
    case 3:
      // The team is made when step 2 is done; until a snapshot lists it,
      // step 2's screen stays.
      if (!team) return ownerScreen(snap);
      if (team.ai?.signed_in === true && teamReady(team)) return "account-signed-in";
      if (team.ai?.signin === "waiting") return "account-waiting";
      if (team.ai?.signin === "closed") return "account-closed";
      return teamReady(team) ? "account-ready" : "account-preparing";
    case 4:
      return kindScreen;
  }
}

function ownerScreen(snap: Pick<Snapshot, "owner_signin">): SetupScreen {
  return ({ idle: "owner", waiting: "owner-waiting", signed_in: "owner-signed-in", failed: "owner-failed" } as const)[snap.owner_signin.state];
}

/** The step label in the footer; create-failed has none. */
export function stepOf(screen: SetupScreen): number | null {
  return STEP_OF[screen];
}

const STEP_OF: Readonly<Record<SetupScreen, number | null>> = {
  kind: 1,
  "kind-work": 1,
  "kind-personal": 1,
  owner: 2,
  "owner-waiting": 2,
  "owner-signed-in": 2,
  "owner-failed": 2,
  "account-preparing": 3,
  "account-ready": 3,
  "account-waiting": 3,
  "account-signed-in": 3,
  "account-closed": 3,
  "done-work": 4,
  "done-personal": 4,
  "create-failed": null,
};

/** Back: where it goes, and whether the team made so far goes with it. */
export function backFrom(s: SetupState): { step: Step | "welcome"; abandon: boolean } {
  if (s.step === 1) return { step: "welcome", abandon: !!s.teamId };
  return { step: (s.step - 1) as Step, abandon: s.step <= 3 && !!s.teamId };
}

/** Continue on step 2: reuse the team when its name, kind and how the
 *  agents call the person didn't change. */
export function needsCreate(s: SetupState): "create" | "reuse" | "recreate" {
  if (!s.teamId || !s.created) return "create";
  const same = s.created.name === s.name.trim() && s.created.kind === s.kind && s.created.callMe === s.callMe.trim();
  return same ? "reuse" : "recreate";
}

export const CALL_ME_HINT: Record<Kind, string> = {
  work: "Optional. For example: Jane, Dr. Patel, CEO",
  personal: "Optional. For example: Jane, Mom, Dad",
};

export const NAME_HINT: Record<Kind, string> = {
  work: "For example: Plainsong, Northwind Legal, Q4 launch",
  personal: "For example: Home, The Parkers, Garden",
};

export interface ReadyBar {
  state: "working" | "ready";
  title: string;
  stage: string | null;
  remaining: string | null;
  percent: number;
}

/** The getting-ready bar above the footer. Null when there is nothing to say. */
export function readyBar(team: Team | undefined | null): ReadyBar | null {
  if (!team) return null;
  if (teamReady(team)) return { state: "ready", title: `${team.name} is ready`, stage: null, remaining: null, percent: 100 };
  const op = team.op;
  if (!op || !op.running) return null;
  return {
    state: "working",
    title: `Getting ${team.name} ready`,
    stage: op.stage,
    remaining: op.remaining,
    percent: clampPercent(op.percent),
  };
}

export function clampPercent(p: number): number {
  if (!Number.isFinite(p)) return 0;
  return Math.max(0, Math.min(100, Math.round(p)));
}

/** The create-failed box: the last few lines then the error, indented. */
export function failureText(op: OpView, all: boolean): string {
  const lines = all ? op.lines : op.lines.slice(-3);
  const out = [...lines];
  if (op.error) out.push(`  ${op.error}`);
  return out.join("\n");
}

/** "Opens when Plainsong is ready · about 40 seconds". */
export function opensWhen(team: Team | undefined | null, name: string): string {
  const rem = team?.op?.running ? team.op.remaining : null;
  return rem ? `Opens when ${name} is ready · ${rem}` : `Opens when ${name} is ready`;
}

/** Thrown errors arrive as strings or as objects; this reads either. */
export function errorText(e: unknown): string {
  if (typeof e === "string") return e;
  if (e instanceof Error) return e.message;
  if (e && typeof e === "object" && "message" in e && typeof (e as { message: unknown }).message === "string")
    return (e as { message: string }).message;
  return JSON.stringify(e);
}

/** The connect page's line for each way the check fails. */
export function connectErrorText(e: ConnectError): string {
  if (e.kind === "unreachable") return "Couldn’t reach this address. Check that the computer or server is on and online.";
  if (e.kind === "not_kivali") return "This address answered, but it isn’t a Kivali team.";
  return e.message || "That isn’t an address Kivali can use.";
}

/** The owner-failed line: why the owner sign-in failed, in ownersignin.rs's words.
 * "timeout" (and nothing) is the browser never coming back. */
export function ownerSigninErrorText(error: string | null): string {
  if (!error || error === "timeout") return "The browser didn’t come back to Kivali. Try again, and keep this window open.";
  return `Sign-in failed: ${error.replace(/\.$/, "")}.`;
}
