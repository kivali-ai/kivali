import { describe, expect, it } from "vitest";
import {
  agentsWorking,
  claudeLine,
  cpuOptions,
  deleteList,
  pauseText,
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
  measuredStep,
  memoryUse,
  memoryWarning,
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
  it("waking, while the team's data moves: the bar's caption says how much", () => {
    const moving = op({
      kind: "resume",
      stage: "Updating team data",
      stages: ["Updating team data", "Starting up", "Setting up your team"],
      percent: 30,
      remaining: "about 80 seconds",
      stage_progress: { done: 3.2 * 1024 ** 3, total: 8 * 1024 ** 3, unit: "bytes" },
    });
    expect(teamPage(team({ name: "Home", state: "starting", activity: "waking", op: moving }), snap, now)).toEqual({
      kind: "progress",
      title: "Waking Home up",
      body: "About 80 seconds.",
      percent: 30,
      step: "Updating team data · 3.2 of 8 GB",
    });
    const compacting = op({ kind: "resume", stage: "Making room", percent: 85, stage_progress: { done: 40, total: 100, unit: "share" } });
    const v = teamPage(team({ state: "starting", activity: "waking", op: compacting }), snap, now);
    expect(v.kind === "progress" && v.step).toBe("Making room · 40%");
    // No caption without a measure, once the operation has ended, or for
    // a measure with no total.
    expect(measuredStep(op({ kind: "resume", stage: "Starting up" }))).toBeNull();
    expect(measuredStep({ ...moving, running: false })).toBeNull();
    expect(measuredStep(op({ stage: "Making room", stage_progress: { done: 0, total: 0, unit: "share" } }))).toBeNull();
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
  // The download, the snapshot's copy and the agents' start say how far
  // they have got, and a failed update says it is going back. Without it
  // the caption showed only a step name through minutes of copying, and
  // "Starting up · step 4 of 4" while the update went back to the version
  // it was on.
  it("updating, with the measured step and the way back", () => {
    const update = { state: "available" as const, current: "0.16.0", latest: "0.17.0", notes_url: null, summary: null, checked_at: null, takes: "about 5 minutes" };
    const stages = ["Downloading", "Saving a snapshot", "Installing", "Starting up", "Starting your agents"];
    const downloading = op({ kind: "update", stages, stage: "Downloading", stage_index: 0, percent: 12, stage_progress: { done: 256 * 1024 ** 2, total: 512 * 1024 ** 2, unit: "bytes" } });
    let v = teamPage(team({ state: "starting", activity: "updating", update, op: downloading }), snap, now);
    expect(v.kind === "progress" && v.step).toBe("Downloading · 256 of 512 MB · step 1 of 5");
    const copying = op({ kind: "update", stages, stage: "Saving a snapshot", stage_index: 1, percent: 40, stage_progress: { done: 3.2 * 1024 ** 3, total: 8 * 1024 ** 3, unit: "bytes" } });
    v = teamPage(team({ state: "starting", activity: "updating", update, op: copying }), snap, now);
    expect(v.kind === "progress" && v.step).toBe("Saving a snapshot · 3.2 of 8 GB · step 2 of 5");
    const agents = op({ kind: "update", stages, stage: "Starting your agents", stage_index: 4, percent: 90, stage_progress: { done: 3, total: 5, unit: "agents" } });
    v = teamPage(team({ state: "starting", activity: "updating", update, op: agents }), snap, now);
    expect(v.kind === "progress" && v.step).toBe("Starting your agents · 3 of 5 · step 5 of 5");
    const back = ["Downloading", "Saving a snapshot", "Installing", "Going back to the previous version"];
    const rolling = op({ kind: "update", stages: back, stage: "Going back to the previous version", stage_index: 3, percent: 80 });
    v = teamPage(team({ state: "starting", activity: "updating", update, op: rolling }), snap, now);
    expect(v.kind === "progress" && v.step).toBe("Going back to the previous version · step 4 of 4");
  });
  it("pausing, in the same voice", () => {
    const v = teamPage(team({ state: "starting", activity: "pausing", op: op({ kind: "pause" }) }), snap, now);
    expect(v.kind === "progress" && v.title).toBe("Pausing Plainsong");
  });
  // A restart would otherwise fall through to "Waking Plainsong up".
  it("restarting, in the same voice", () => {
    const v = teamPage(team({ state: "starting", activity: "restarting", op: op({ kind: "restart" }) }), snap, now);
    expect(v.kind === "progress" && v.title).toBe("Restarting Plainsong");
    expect(v.kind === "progress" && v.body).toBe("Agents pick up where they left off.");
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
    // The memory line names this computer as its platform does.
    const w = teamPage(t, snapshot({ platform: "windows", teams: [t, home] }), now);
    expect(w.kind === "failed" && w.body).toBe("This PC is low on memory. Home is using 4 GB.");
    expect(statusRow(t, snapshot({ platform: "windows", teams: [t, home] }), now).sub).toBe("This PC is low on memory. Home is using 4 GB.");
  });
  it("can't start while this computer can't run the team's VM: the steps, no button", () => {
    const off = snapshot({ platform: "windows", host: { memory_mb: 16384, cpus: 8, vm_support: "hyperv_off" } });
    for (const state of ["paused", "failed"] as const) {
      const v = teamPage(team({ state, reason: "Hyper-V is turned off on this PC." }), off, now);
      expect(v.kind).toBe("vm");
      if (v.kind !== "vm") continue;
      expect(v.title).toBe("Plainsong can’t start");
      expect(v.help.title).toBe("Turn on Hyper-V to run a team on this PC");
      expect(v.help.steps.length).toBeGreaterThan(0);
    }
    // A running team, a team elsewhere and one that is starting show as before.
    expect(teamPage(team({ state: "running" }), off, now).kind).toBe("progress");
    expect(teamPage(team({ place: "elsewhere", device: "dana-imac.local", state: "paused" }), off, now).kind).toBe("unreachable");
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
    expect(statusRow(team(), snap, now)).toMatchObject({ word: "Running", sub: "Agents can work", action: "pause", actionLabel: "Pause…", restart: false });
    expect(statusRow(team({ ai: { provider: "claude", signed_in: false, email: null, billing: null, terminal_open: false, signin: "idle", signed_out_at: null } }), snap, now).sub).toBe(
      "Claude isn’t signed in",
    );
    // The status row: "Running · 3 agents working", from the team's own API.
    expect(statusRow(team({ working: 3 }), snap, now).sub).toBe("3 agents working");
    expect(statusRow(team({ working: 1 }), snap, now).sub).toBe("1 agent working");
    expect(statusRow(team({ working: 0 }), snap, now).sub).toBe("Agents can work");
    expect(statusRow(team({ state: "paused" }), snap, now)).toMatchObject({ action: "resume", actionLabel: "Resume", restart: false });
    expect(statusRow(team({ state: "starting", activity: "updating", op: op({ stage: "Installing", remaining: "about 1 minute" }) }), snap, now)).toMatchObject({
      word: "Updating",
      sub: "Installing · about 1 minute",
      action: null,
    });
  });
  // Restart shows only beside Pause on a development build: never on a
  // release, and never while the team is paused, starting or failed.
  it("status row offers Restart on a development build while running", () => {
    const snap = snapshot();
    expect(statusRow(team({ dev: true, version: "0.0.0-dev" }), snap, now)).toMatchObject({ action: "pause", restart: true });
    expect(statusRow(team({ dev: true, state: "paused" }), snap, now).restart).toBe(false);
    expect(statusRow(team({ dev: true, state: "failed" }), snap, now).restart).toBe(false);
    expect(statusRow(team({ dev: true, state: "starting", activity: "restarting", op: op({ kind: "restart" }) }), snap, now)).toMatchObject({ word: "Restarting", restart: false });
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
      failed: null,
    });
    expect(updateRow(team({ version: null, update: null }), now)).toBeNull();
  });
  it("update row after a failed update", () => {
    const update = { state: "available" as const, current: "1.0.1", latest: "1.1.1", notes_url: null, summary: null, checked_at: null, takes: "about 3 minutes" };
    const error = "image docker.io/library/kivali-browser:v1.1.1 is missing";
    const t = team({ name: "Home", version: "1.0.1", update, op: op({ kind: "update", running: false, error }) });
    expect(updateRow(t, now)).toEqual({
      title: "Kivali 1.0.1",
      sub: "Couldn’t update to 1.1.1",
      highlight: true,
      update: true,
      notes: null,
      failed: { title: "Home couldn’t update to 1.1.1", body: `It’s still on 1.0.1. The update stopped: ${error}` },
    });
    // Only a finished update's error counts: one still running, or
    // another operation's, is no update failure.
    expect(updateRow(team({ update, op: op({ kind: "update", running: true, error }) }), now)?.failed).toBeNull();
    expect(updateRow(team({ update, op: op({ kind: "resume", running: false, error }) }), now)?.failed).toBeNull();
    // Once the update is no longer on offer, the failure is not shown.
    expect(updateRow(team({ update: { ...update, state: "up_to_date" }, op: op({ kind: "update", running: false, error }) }), now)?.failed).toBeNull();
  });
  // A team that needs a newer app offers the app's update on its own row
  // (E4, as General's Update…). Without it the row only names a setting
  // elsewhere, and a team stays behind on an update the person never saw.
  it("update row when the app must update first", () => {
    const update = { state: "desktop_first" as const, current: "1.1.1", latest: "1.2.0", notes_url: null, summary: null, checked_at: null, takes: "about 3 minutes" };
    expect(updateRow(team({ version: "1.1.1", update }), now)).toEqual({
      title: "Kivali 1.1.1",
      sub: "1.2.0 is available · update this app first",
      highlight: true,
      update: false,
      app: true,
      notes: null,
      failed: null,
    });
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
  // Guards the memory setting's warning: a team below what a web task needs (an agent and a browser beside the
  // server, the shell's web_memory_mb) has every web task fail short of memory, and 2 GB is still offered.
  it("memory warning below what a web task needs", () => {
    expect(memoryWarning(team({ name: "Home", memory_mb: 2048 }), 2122)).toBe("Web tasks need at least 3 GB. With 2 GB, Home can’t start a browser for them.");
    expect(memoryWarning(team({ memory_mb: 3072 }), 2122)).toBeNull();
    expect(memoryWarning(team({ memory_mb: 4096 }), 2122)).toBeNull();
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
  it("the pause dialog says how many agents it stops", () => {
    expect(pauseText(team({ working: 3 }))).toBe("3 agents are working. They stop mid-task and pick up where they left off when you resume.");
    expect(pauseText(team({ working: 1 }))).toBe("1 agent is working. It stops mid-task and picks up where it left off when you resume.");
    expect(pauseText(team())).toBe("Agents that are working stop mid-task and pick up where they left off when you resume.");
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
    expect(devicesHint({ url: "http://127.0.0.1:18080" }, "macos").join("")).toBe(
      "Kivali doesn’t set this up. Put this Mac’s team behind https yourself, for example with tailscale serve, pointing at http://127.0.0.1:18080.",
    );
    expect(devicesHint({ url: "http://127.0.0.1:18080" }, "windows")[0]).toBe("Kivali doesn’t set this up. Put this PC’s team behind https yourself, for example with ");
    expect(devicesHint({ url: null }, "macos")[2]).toBe(", pointing at its local address.");
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
  // The app's download moves its bytes without a shell-changed event (one
  // per chunk would redraw every menu thousands of times). Without polling,
  // Settings → General's bar would sit at 0% until the download ended.
  it("while the app's update downloads", () => {
    const app_update = { state: "downloading" as const, version: "1.2.0", checked_at: null, error: null, downloaded: 1, total: 2 };
    expect(shouldPoll(snapshot({ app_update }))).toBe(true);
  });
});
