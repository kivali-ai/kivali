import { describe, expect, it } from "vitest";
import {
  backFrom,
  connectErrorText,
  failureText,
  needsCreate,
  newSetup,
  opensWhen,
  ownerSigninErrorText,
  readyBar,
  resumeSetup,
  retryCommand,
  setupScreen,
  stepOf,
  type SetupState,
} from "./setup";
import { op, snapshot, team } from "./testdata";

const st = (o: Partial<SetupState>): SetupState => ({ ...newSetup(), ...o });
const creating = team({ id: "n", state: "starting", activity: "creating", provisional: true, op: op({ stage: "Starting up", percent: 45, remaining: "about 40 seconds" }) });
const ready = (ai: Partial<NonNullable<ReturnType<typeof team>["ai"]>> = {}) =>
  team({
    id: "n",
    op: op({ running: false, finished: true, percent: 100 }),
    ai: { provider: "claude", signed_in: false, email: null, billing: null, terminal_open: false, signin: "idle", signed_out_at: null, ...ai },
  });

describe("setupScreen", () => {
  const snap = snapshot();
  it("step 1: the cards, then the name", () => {
    expect(setupScreen(st({ step: 1 }), snap)).toBe("kind");
    expect(setupScreen(st({ step: 1, kind: "work" }), snap)).toBe("kind-work");
    expect(setupScreen(st({ step: 1, kind: "personal" }), snap)).toBe("kind-personal");
  });
  it("step 2 follows the owner's Google sign-in", () => {
    const s = st({ step: 2 });
    expect(setupScreen(s, snapshot({ owner_signin: { state: "idle", email: null, error: null } }))).toBe("owner");
    expect(setupScreen(s, snapshot({ owner_signin: { state: "waiting", email: null, error: null } }))).toBe("owner-waiting");
    expect(setupScreen(s, snapshot({ owner_signin: { state: "signed_in", email: "a@b.co", error: null } }))).toBe("owner-signed-in");
    expect(setupScreen(s, snapshot({ owner_signin: { state: "failed", email: null, error: "x" } }))).toBe("owner-failed");
  });
  it("step 3, Claude's sign-in: not ready, ready, waiting, signed in, closed", () => {
    const s = st({ step: 3, teamId: "n" });
    expect(setupScreen(s, snapshot({ teams: [creating] }))).toBe("account-preparing");
    expect(setupScreen(s, snapshot({ teams: [ready()] }))).toBe("account-ready");
    expect(setupScreen(s, snapshot({ teams: [ready({ signin: "waiting", terminal_open: true })] }))).toBe("account-waiting");
    expect(setupScreen(s, snapshot({ teams: [ready({ signed_in: true, billing: "Amazon Bedrock" })] }))).toBe("account-signed-in");
    expect(setupScreen(s, snapshot({ teams: [ready({ signin: "closed" })] }))).toBe("account-closed");
    // Signed in, but the team not ready yet: still getting ready.
    expect(setupScreen(s, snapshot({ teams: [{ ...creating, ai: ready({ signed_in: true }).ai }] }))).toBe("account-preparing");
  });
  it("step 4 by kind, and create-failed whenever the create op failed", () => {
    expect(setupScreen(st({ step: 4, kind: "work" }), snap)).toBe("done-work");
    expect(setupScreen(st({ step: 4, kind: "personal" }), snap)).toBe("done-personal");
    const failed = team({ id: "n", state: "failed", op: op({ running: false, finished: true, error: "boom" }) });
    expect(setupScreen(st({ step: 3, teamId: "n" }), snapshot({ teams: [failed] }))).toBe("create-failed");
  });
  it("create-failed too when restarting a team whose setup was left unfinished fails", () => {
    const s = st({ step: 3, teamId: "n" });
    const resumeFailed = team({ id: "n", state: "failed", provisional: true, op: op({ kind: "resume", running: false, finished: true, error: "boom" }) });
    expect(setupScreen(s, snapshot({ teams: [resumeFailed] }))).toBe("create-failed");
    // Refused before an operation ran (memory, a computer that can't run the VM).
    const refused = team({ id: "n", state: "failed", provisional: true, op: null, reason: "Hyper-V is turned off on this PC." });
    expect(setupScreen(s, snapshot({ teams: [refused] }))).toBe("create-failed");
  });
  it("a team not listed yet keeps step 2's screen", () => {
    const signedIn = snapshot({ owner_signin: { state: "signed_in", email: "a@b.co", error: null } });
    expect(setupScreen(st({ step: 3, teamId: "gone" }), signedIn)).toBe("owner-signed-in");
  });
});

describe("picking setup up again", () => {
  it("starts at Claude's sign-in for the team setup made", () => {
    const left = team({ id: "home-1", name: "Home", kind: "personal", provisional: true, state: "paused" });
    expect(resumeSetup(left)).toMatchObject({ step: 3, kind: "personal", name: "Home", teamId: "home-1", created: { name: "Home", kind: "personal", callMe: "" } });
    // Back from there removes the team, as it does in one sitting.
    expect(backFrom(resumeSetup(left))).toEqual({ step: 2, abandon: true });
  });
  it("tries again by creating afresh in the same sitting, by starting the team otherwise", () => {
    expect(retryCommand(team({ op: op({ kind: "create", running: false, error: "x" }) }))).toBe("retry_create");
    expect(retryCommand(team({ op: op({ kind: "resume", running: false, error: "x" }) }))).toBe("retry_team");
    expect(retryCommand(team({ op: null }))).toBe("retry_team");
  });
});

describe("footer and Back", () => {
  it("step labels", () => {
    expect(["kind", "owner-failed", "account-preparing", "account-signed-in", "done-personal"].map((s) => stepOf(s as never))).toEqual([1, 2, 3, 3, 4]);
    expect(stepOf("create-failed")).toBeNull();
  });
  it("Back from step 3 or earlier abandons the team", () => {
    expect(backFrom(st({ step: 3, teamId: "n" }))).toEqual({ step: 2, abandon: true });
    expect(backFrom(st({ step: 2, teamId: "n" }))).toEqual({ step: 1, abandon: true });
    expect(backFrom(st({ step: 3 }))).toEqual({ step: 2, abandon: false });
    expect(backFrom(st({ step: 1 }))).toEqual({ step: "welcome", abandon: false });
  });
  it("Continue on step 2 reuses the team when nothing changed", () => {
    expect(needsCreate(st({ name: "A" }))).toBe("create");
    const made = st({ name: "A", kind: "work", teamId: "n", created: { name: "A", kind: "work", callMe: "" } });
    expect(needsCreate(made)).toBe("reuse");
    expect(needsCreate({ ...made, name: "B" })).toBe("recreate");
    // What the agents call the person is an install value: changing it starts over.
    expect(needsCreate({ ...made, callMe: "Mom" })).toBe("recreate");
    expect(needsCreate({ ...made, callMe: "  " })).toBe("reuse");
  });
});

describe("the getting-ready bar", () => {
  it("working, then ready", () => {
    expect(readyBar(creating)).toEqual({ state: "working", title: "Getting Plainsong ready", stage: "Starting up", remaining: "about 40 seconds", percent: 45 });
    expect(readyBar(ready())?.title).toBe("Plainsong is ready");
    expect(readyBar(null)).toBeNull();
    expect(readyBar(team({ id: "n", state: "starting", op: op({ percent: 140 }) }))?.percent).toBe(100);
  });
  it("the getting-ready bar's caption", () => {
    expect(opensWhen(creating, "Plainsong")).toBe("Opens when Plainsong is ready · about 40 seconds");
    expect(opensWhen(team({ op: op({ remaining: null }) }), "Plainsong")).toBe("Opens when Plainsong is ready");
  });
  it("the create-failed box: last lines then the indented error", () => {
    const t = team({ op: op({ lines: ["a", "b", "c", "d"], error: "image pull timed out" }) });
    expect(failureText(t, false)).toBe("b\nc\nd\n  image pull timed out");
    expect(failureText(t, true)).toBe("a\nb\nc\nd\n  image pull timed out");
    // A start refused before any operation ran: the team's reason.
    expect(failureText(team({ op: null, reason: "This PC is low on memory." }), false)).toBe("This PC is low on memory.");
  });
});

describe("error words", () => {
  it("addresses", () => {
    expect(connectErrorText({ kind: "not_kivali", message: "" })).toBe("This address answered, but it isn’t a Kivali team.");
    expect(connectErrorText({ kind: "unreachable", message: "" })).toMatch(/^Couldn’t reach this address\./);
  });
  it("owner sign-in", () => {
    expect(ownerSigninErrorText("timeout")).toMatch(/^The browser didn’t come back/);
    expect(ownerSigninErrorText(null)).toMatch(/^The browser didn’t come back/);
    expect(ownerSigninErrorText("the sign-in service answered 401 (invalid_client: The provided client secret is invalid.)")).toBe(
      "Sign-in failed: the sign-in service answered 401 (invalid_client: The provided client secret is invalid.).",
    );
  });
});
