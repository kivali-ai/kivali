import { describe, expect, it } from "vitest";
import {
  agentsWorking,
  claudeLine,
  cpuOptions,
  deleteList,
  signedOutLine,
  updateText,
  devicesHint,
  devicesView,
  diskLines,
  signInLine,
  dotOf,
  memoryConflict,
  memoryDialog,
  memoryOptions,
  memoryUse,
  osPromptLine,
  placeLine,
  shouldPoll,
  statusRow,
  teamPage,
  updateRow,
} from "./teams";
import { op, snapshot, team } from "./testdata";

const now = new Date(2026, 9, 5, 15, 0);
const GB = 1024 ** 3;

describe("dots", () => {
  it("always come with a word", () => {
    expect(dotOf("running")).toEqual({ cls: "dot--running", word: "Running" });
    expect(dotOf("starting").cls).toBe("dot--starting");
    expect(dotOf("paused").word).toBe("Paused");
    expect(dotOf("failed").word).toBe("Couldn’t start");
  });
  it("place line", () => {
    expect(placeLine(team(), "macos")).toBe("Work · on this Mac");
    expect(placeLine(team({ kind: "personal" }), "windows")).toBe("Personal · on this PC");
    expect(placeLine(team({ place: "elsewhere", device: "dana-imac.local" }), "macos")).toBe("Work · on Dana’s iMac");
    expect(placeLine(team({ kind: null }), "macos")).toBe("On this Mac");
  });
});

describe("teamPage", () => {
  const snap = snapshot();
  it("paused", () => {
    const v = teamPage(team({ name: "Home", state: "paused", paused_since: new Date(2026, 9, 5, 9, 14).toISOString() }), snap, now);
    expect(v).toEqual({ kind: "paused", title: "Home is paused", body: "Your agents aren’t working. Nothing is lost.", action: "Resume Home", since: "Paused since 9:14 this morning" });
  });
  it("waking", () => {
    const v = teamPage(team({ name: "Home", state: "starting", activity: "waking", op: op({ kind: "resume", percent: 40, remaining: "about 30 seconds" }) }), snap, now);
    expect(v).toEqual({ kind: "progress", title: "Waking Home up", body: "About 30 seconds.", percent: 40, step: null });
  });
  it("updating, with the step line", () => {
    const t = team({
      state: "starting",
      activity: "updating",
      update: { state: "available", current: "0.16.0", latest: "0.17.0", notes_url: null, summary: null, checked_at: null, takes: "about 3 minutes" },
      op: op({ kind: "update", stages: ["Downloading", "Saving a snapshot", "Installing", "Starting up"], stage: "Saving a snapshot", stage_index: 1, percent: 35, remaining: "about 3 minutes" }),
    });
    expect(teamPage(t, snap, now)).toEqual({
      kind: "progress",
      title: "Updating Plainsong to 0.17",
      body: "About 3 minutes. Agents pick up where they left off.",
      percent: 35,
      step: "Saving a snapshot · step 2 of 4",
    });
  });
  it("pausing, in the same voice", () => {
    const v = teamPage(team({ state: "starting", activity: "pausing", op: op({ kind: "pause" }) }), snap, now);
    expect(v.kind === "progress" && v.title).toBe("Pausing Plainsong");
  });
  it("couldn't start, with a team to pause first, or the generic reason", () => {
    const home = team({ id: "home", name: "Home", memory_mb: 4096 });
    const t = team({ state: "failed", pause_first: "home", op: op({ kind: "resume", running: false, error: "no memory", lines: ["x"] }) });
    const v = teamPage(t, snapshot({ teams: [t, home] }), now);
    expect(v).toEqual({
      kind: "failed",
      title: "Plainsong couldn’t start",
      body: "This Mac is low on memory. Home is using 4 GB.",
      action: { label: "Pause Home and try again", pause: "home" },
      details: "x\nno memory",
    });
    const g = teamPage(team({ state: "failed" }), snap, now);
    expect(g.kind === "failed" && [g.body, g.action.label, g.details]).toEqual(["Something went wrong while starting.", "Try again", null]);
    const r = teamPage(team({ state: "failed", reason: "the disk is full" }), snap, now);
    expect(r.kind === "failed" && r.body).toBe("The disk is full.");
  });
  it("can't reach", () => {
    const t = team({ name: "Studio", place: "elsewhere", device: "dana-imac.local", state: "paused", last_reached: new Date(2026, 9, 5, 13, 0).toISOString() });
    expect(teamPage(t, snap, now)).toEqual({
      kind: "unreachable",
      title: "Can’t reach Dana’s iMac",
      body: "Studio runs there. It may be asleep or offline.",
      last: "Last reached 2 hours ago",
    });
  });
  it("a team that's gone", () => {
    expect(teamPage(undefined, snap, now).kind).toBe("gone");
  });
});

describe("Settings rows", () => {
  it("status row per state", () => {
    const snap = snapshot();
    expect(statusRow(team(), snap, now)).toMatchObject({ word: "Running", sub: "Agents can work", action: "pause", actionLabel: "Pause…" });
    expect(statusRow(team({ ai: { provider: "claude", signed_in: false, email: null, billing: null, terminal_open: false, signin: "idle", signed_out_at: null } }), snap, now).sub).toBe(
      "Claude isn’t signed in",
    );
    // The status row: "Running · 3 agents working", from the team's own API.
    expect(statusRow(team({ working: 3 }), snap, now).sub).toBe("3 agents working");
    expect(statusRow(team({ working: 1 }), snap, now).sub).toBe("1 agent working");
    expect(statusRow(team({ working: 0 }), snap, now).sub).toBe("Agents can work");
    expect(statusRow(team({ state: "paused" }), snap, now)).toMatchObject({ action: "resume", actionLabel: "Resume" });
    expect(statusRow(team({ state: "starting", activity: "updating", op: op({ stage: "Installing", remaining: "about 1 minute" }) }), snap, now)).toMatchObject({
      word: "Updating",
      sub: "Installing · about 1 minute",
      action: null,
    });
  });
  it("update row", () => {
    const t = team({
      version: "0.16.0",
      update: { state: "available", current: "0.16.0", latest: "0.17.0", notes_url: "https://n", summary: "new knowledge graph view, faster startup", checked_at: null, takes: "about 3 minutes" },
    });
    expect(updateRow(t, now)).toEqual({
      title: "Kivali 0.16.0",
      sub: "0.17.0 is available · new knowledge graph view, faster startup",
      highlight: true,
      update: true,
      notes: "https://n",
    });
    expect(updateRow(team({ version: null, update: null }), now)).toBeNull();
  });
  it("memory bar", () => {
    const a = team({ id: "a", name: "Plainsong" });
    const b = team({ id: "b", name: "Home" });
    const c = team({ id: "c", name: "Paused", state: "paused" });
    const u = memoryUse(b, snapshot({ teams: [a, b, c] }));
    expect(u.label).toBe("Memory on this Mac · 2 teams running · 8 of 16 GB");
    expect(u.segments.map((s) => [s.name, s.pct, s.self])).toEqual([
      ["Home", 25, true],
      ["Plainsong", 25, false],
    ]);
    expect(u.freeMb).toBe(8192);
  });
  it("resource options", () => {
    expect(memoryOptions(16384, 4096).map((o) => o.label)).toEqual(["4 GB", "6 GB", "8 GB"]);
    expect(memoryOptions(16384, 3072).map((o) => o.label)).toEqual(["3 GB", "4 GB", "6 GB", "8 GB"]);
    expect(cpuOptions(8, 4).map((o) => o.label)).toEqual(["2", "4", "6", "8"]);
    expect(cpuOptions(4, 4).map((o) => o.label)).toEqual(["2", "4"]);
  });
  it("disk card", () => {
    expect(diskLines(team({ disk_used_bytes: 18 * GB, disk_size_bytes: 64 * GB }))).toEqual({
      title: "18 GB used",
      sub: "Of 64 GB reserved for Plainsong. Only what’s used takes space.",
    });
    expect(diskLines(team()).title).toBe("Not measured yet");
  });
});

describe("dialogs", () => {
  it("the delete dialog drops numbers it doesn't have", () => {
    expect(deleteList(team({ disk_used_bytes: 18 * GB }), { agents: 6, files: 1204, disk_used_bytes: null }, "macos")).toEqual([
      "6 agents and what they remember",
      "1,204 files",
      "Every chat and assignment",
      "The Claude sign-in",
      "18 GB on this Mac",
    ]);
    expect(deleteList(team(), null, "macos")).toEqual([
      "Its agents and what they remember",
      "Its files",
      "Every chat and assignment",
      "The Claude sign-in",
      "Its disk on this Mac",
    ]);
  });
  it("the memory dialog from resume's memory error", () => {
    expect(memoryConflict("memory:plainsong")).toBe("plainsong");
    expect(memoryConflict("something else")).toBeNull();
    const d = memoryDialog(team({ name: "Home" }), team({ name: "Plainsong" }), "macos");
    expect(d.title).toBe("Not enough memory for Home");
    expect(d.confirm).toBe("Pause Plainsong and resume");
    expect(d.body).toBe("Home needs 4 GB, and this Mac doesn’t have that free while Plainsong runs. Pause Plainsong to resume Home?");
    // With the shell's own estimate, as the native alert says it.
    expect(memoryDialog(team({ name: "Home", memory_free_mb: 2048 }), team({ name: "Plainsong" }), "macos").body).toBe(
      "Home needs 4 GB, and this Mac has 2 GB free while Plainsong runs. Pause Plainsong to resume Home?",
    );
  });
  it("the delete dialog falls back to the snapshot's numbers", () => {
    expect(deleteList(team({ agents: 1, files: 1 }), null, "macos").slice(0, 2)).toEqual(["1 agent and what they remember", "1 file"]);
    expect(deleteList(team({ agents: 1 }), { agents: 6, files: null, disk_used_bytes: null }, "macos")[0]).toBe("6 agents and what they remember");
  });
  it("the update dialog says how long", () => {
    const t = team({
      version: "0.16.0",
      update: { state: "available", current: "0.16.0", latest: "0.17.0", notes_url: null, summary: null, checked_at: null, takes: "about 3 minutes" },
    });
    expect(updateText(t)).toBe("It takes about 3 minutes. Agents pause while it updates. If anything goes wrong, Plainsong goes back to 0.16.");
  });
  it("the signed-out date and the signed-in line", () => {
    const ai = { provider: "claude" as const, signed_in: false, email: null, billing: null, terminal_open: false, signin: "idle" as const, signed_out_at: null };
    expect(signedOutLine(team({ ai: { ...ai, signed_out_at: new Date(2026, 9, 2, 10).toISOString() } }), now)).toBe("Claude signed out on 2 Oct.");
    expect(signedOutLine(team({ ai }), now)).toBeNull();
    const on = { ...ai, signed_in: true };
    expect(claudeLine(team({ ai: { ...on, email: "dana@example.com", billing: "Claude Max" } }))).toBe("dana@example.com · Claude Max");
    expect(claudeLine(team({ ai: { ...on, billing: "Amazon Bedrock" } }))).toBe("Amazon Bedrock");
    expect(claudeLine(team({ ai: on }))).toBe("Signed in");
  });
  it("the working phrase", () => {
    expect(agentsWorking(3)).toBe("3 agents working");
    expect(agentsWorking(0)).toBeNull();
    expect(agentsWorking(null)).toBeNull();
  });
  it("the delete confirmation's OS prompt line", () => {
    expect(osPromptLine("macos")).toBe("Next, your Mac asks for your password or Touch ID.");
  });
});

describe("Other devices", () => {
  const off = { revealed: false, editing: false };
  it("which state shows", () => {
    expect(devicesView({ public_url: null }, off)).toBe("off");
    expect(devicesView({ public_url: null }, { ...off, revealed: true })).toBe("form");
    expect(devicesView({ public_url: "https://a.ts.net" }, off)).toBe("on");
    expect(devicesView({ public_url: "https://a.ts.net" }, { ...off, editing: true })).toBe("form");
  });
  it("the hint names the local address, or says it hasn't one", () => {
    expect(devicesHint({ url: "http://127.0.0.1:18080" }).join("")).toBe(
      "Kivali doesn’t set this up. Put this Mac’s team behind https yourself, for example with tailscale serve, pointing at http://127.0.0.1:18080.",
    );
    expect(devicesHint({ url: null })[2]).toBe(", pointing at its local address.");
  });
  it("who can sign in", () => {
    expect(signInLine({ owner: "dana@example.com" })).toBe("dana@example.com");
    expect(signInLine({ owner: null })).toBe("The team's owner");
  });
});

describe("shouldPoll", () => {
  it("while something moves", () => {
    expect(shouldPoll(snapshot())).toBe(false);
    expect(shouldPoll(snapshot({ teams: [team({ op: op() })] }))).toBe(true);
    expect(shouldPoll(snapshot({ owner_signin: { state: "waiting", email: null, error: null } }))).toBe(true);
    expect(shouldPoll(snapshot({ teams: [team({ ai: { provider: "claude", signed_in: false, email: null, billing: null, terminal_open: true, signin: "waiting", signed_out_at: null } })] }))).toBe(true);
  });
});
