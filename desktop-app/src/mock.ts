// Dev only (`npm run dev` in a plain browser): answers every command from
// fixtures keyed by ?state=<fixture name>, so each state opens on its
// own, e.g. http://localhost:1420/?state=account-preparing#/setup/new . A missing hash
// is filled with the fixture's route. ipc.ts imports this only under
// import.meta.env.DEV without Tauri, so a build leaves it out.

import type { Preset } from "./preset";
import type { OpView, Snapshot, Team } from "./types";

const HOUR = 3_600_000;

function iso(msAgo: number): string {
  return new Date(Date.now() - msAgo).toISOString();
}

function todayAt(h: number, m: number): string {
  const d = new Date();
  d.setHours(h, m, 0, 0);
  if (d.getTime() > Date.now()) d.setTime(d.getTime() - 86_400_000);
  return d.toISOString();
}

function op(o: Partial<OpView>): OpView {
  return {
    kind: "create",
    running: true,
    error: null,
    finished: false,
    lines: [],
    stage: null,
    stage_index: 0,
    stages: [],
    started_ms: Date.now() - 20_000,
    percent: 0,
    remaining: null,
    ...o,
  };
}

const CREATE_STAGES = ["Making room", "Starting up", "Setting up your team", "Ready"];
const UPDATE_STAGES = ["Downloading", "Saving a snapshot", "Installing", "Starting up"];
const CREATE_LINES = [
  "creating the data disk · done",
  "booting the VM · done",
  "installing Kivali 0.17.0 · started",
  "pulling images (2 of 5)",
];

function team(o: Partial<Team> & Pick<Team, "id" | "name">): Team {
  return {
    kind: "work",
    place: "here",
    url: "http://127.0.0.1:18080",
    device: null,
    state: "running",
    activity: null,
    phrase: "running",
    reason: null,
    pause_first: null,
    op: null,
    version: "0.17.0",
    update: { state: "up_to_date", current: "0.17.0", latest: "0.17.0", notes_url: null, summary: null, checked_at: iso(3 * HOUR), takes: "about 3 minutes" },
    ai: { provider: "claude", signed_in: true, email: "dana@example.com", billing: "Claude Max", terminal_open: false, signin: "idle", signed_out_at: null },
    owner: "dana@example.com",
    memory_mb: 4096,
    cpus: 4,
    disk_used_bytes: 18 * 1024 ** 3,
    disk_size_bytes: 64 * 1024 ** 3,
    paused_since: null,
    last_reached: null,
    provisional: false,
    public_url: null,
    agents: null,
    working: null,
    files: null,
    signed_in_as: null,
    memory_free_mb: 8192,
    ...o,
  };
}

/** A day of this year, as the app dates things ("2 Oct", "12 Sep"). */
function onDay(month: number, day: number): string {
  return new Date(new Date().getFullYear(), month - 1, day, 10, 0).toISOString();
}

const plainsong = () =>
  team({ id: "plainsong", name: "Plainsong", phrase: "3 agents working", agents: 6, working: 3, files: 1204, signed_in_as: "dana@example.com" });
const home = () =>
  team({
    id: "home",
    name: "Home",
    kind: "personal",
    state: "paused",
    phrase: "paused",
    url: null,
    paused_since: todayAt(9, 14),
    ai: { provider: "claude", signed_in: true, email: null, billing: "Amazon Bedrock", terminal_open: false, signin: "idle", signed_out_at: null },
    disk_used_bytes: 6 * 1024 ** 3,
    agents: 2,
    files: 38,
    signed_in_as: "dana@example.com",
    // The memory dialog: "this Mac has 2 GB free while Plainsong runs".
    memory_free_mb: 2048,
  });
const studio = () =>
  team({
    id: "studio",
    name: "Studio",
    place: "elsewhere",
    device: "dana-imac.local",
    url: "https://dana-imac.local:8443",
    phrase: "on dana-imac.local",
    signed_in_as: "dana@example.com",
    last_reached: iso(2 * HOUR),
    update: null,
    ai: null,
    memory_free_mb: null,
  });

function base(): Snapshot {
  return {
    app_version: "0.17.1",
    platform: "macos",
    teams: [plainsong(), home(), studio()],
    host: { memory_mb: 16384, cpus: 8 },
    settings: { start_at_login: true, ask_before_quit: true },
    app_update: { state: "up_to_date", version: null, checked_at: iso(3 * HOUR), error: null },
    owner_signin: { state: "signed_in", email: "dana@example.com", error: null },
    connect: { state: "idle", origin: null, name: null, error: null, team_id: null, email: null },
    config_dir: "/Users/dana/Library/Application Support/Kivali",
    logs_dir: "/Users/dana/Library/Logs/Kivali",
    last_deleted: null,
  };
}

/** A provisional team getting ready, for step 4. */
function creating(o: Partial<Team> = {}, opv: Partial<OpView> = {}): Team {
  return team({
    id: "new1",
    name: "Plainsong",
    state: "starting",
    activity: "creating",
    phrase: "getting ready",
    provisional: true,
    version: null,
    update: null,
    op: op({ stage: "Starting up", stage_index: 1, stages: CREATE_STAGES, percent: 45, remaining: "about 40 seconds", lines: CREATE_LINES, ...opv }),
    ai: { provider: "claude", signed_in: false, email: null, billing: null, terminal_open: false, signin: "idle", signed_out_at: null },
    ...o,
  });
}

function ready(o: Partial<Team> = {}): Team {
  return creating({ state: "running", activity: null, phrase: "running", op: op({ running: false, finished: true, percent: 100, stage: "Ready", stage_index: 3, stages: CREATE_STAGES, lines: CREATE_LINES }), ...o });
}

const signedOut = { provider: "claude" as const, signed_in: false, email: null, billing: null, terminal_open: false, signin: "idle" as const, signed_out_at: null };

interface Fixture {
  hash: string;
  snap?: (s: Snapshot) => void;
  preset?: Preset;
}

const step3: Preset = {
  setup: { step: 3, kind: "work", name: "Plainsong", teamId: "new1", created: { name: "Plainsong", kind: "work", callMe: "" } },
};

const FIXTURES: Record<string, Fixture> = {
  "welcome": { hash: "#/setup/welcome", snap: (s) => ((s.teams = []), (s.owner_signin = { state: "idle", email: null, error: null })) },
  "kind": { hash: "#/setup/new", snap: (s) => (s.teams = []), preset: { setup: { step: 1 } } },
  "kind-work": { hash: "#/setup/new", snap: (s) => (s.teams = []), preset: { setup: { step: 1, kind: "work", name: "Plainsong" } } },
  "kind-personal": { hash: "#/setup/new", snap: (s) => (s.teams = []), preset: { setup: { step: 1, kind: "personal", name: "Home" } } },
  "owner": { hash: "#/setup/new", snap: (s) => ((s.teams = []), (s.owner_signin = { state: "idle", email: null, error: null })), preset: { setup: { step: 2, kind: "work", name: "Plainsong" } } },
  "owner-waiting": { hash: "#/setup/new", snap: (s) => ((s.teams = []), (s.owner_signin = { state: "waiting", email: null, error: null })), preset: { setup: { step: 2, kind: "work", name: "Plainsong" } } },
  "owner-signed-in": { hash: "#/setup/new", snap: (s) => (s.teams = []), preset: { setup: { step: 2, kind: "work", name: "Plainsong" } } },
  "owner-failed": {
    hash: "#/setup/new",
    snap: (s) => ((s.teams = []), (s.owner_signin = { state: "failed", email: null, error: "timed out" })),
    preset: { setup: { step: 2, kind: "work", name: "Plainsong" } },
  },
  "account-preparing": { hash: "#/setup/new", snap: (s) => (s.teams = [creating()]), preset: step3 },
  "account-ready": { hash: "#/setup/new", snap: (s) => (s.teams = [ready({ ai: signedOut })]), preset: step3 },
  "account-waiting": { hash: "#/setup/new", snap: (s) => (s.teams = [ready({ ai: { ...signedOut, terminal_open: true, signin: "waiting" } })]), preset: step3 },
  "account-signed-in": {
    hash: "#/setup/new",
    snap: (s) => (s.teams = [ready({ ai: { ...signedOut, signed_in: true, email: "dana@example.com", billing: "Claude Max" } })]),
    preset: step3,
  },
  "account-closed": { hash: "#/setup/new", snap: (s) => (s.teams = [ready({ ai: { ...signedOut, signin: "closed" } })]), preset: step3 },
  "done-work": {
    hash: "#/setup/new",
    snap: (s) => (s.teams = [ready({ ai: { ...signedOut, signed_in: true, email: "dana@example.com", billing: "Claude Max" } })]),
    preset: { setup: { step: 4, kind: "work", name: "Plainsong", teamId: "new1" } },
  },
  "done-personal": {
    hash: "#/setup/new",
    snap: (s) =>
      (s.teams = [
        ready({ name: "Home", kind: "personal", ai: { ...signedOut, signed_in: true, billing: "Google Vertex AI" } }),
      ]),
    preset: { setup: { step: 4, kind: "personal", name: "Home", teamId: "new1" } },
  },
  "create-failed": {
    hash: "#/setup/new",
    snap: (s) =>
      (s.teams = [
        creating(
          { state: "failed", activity: null },
          {
            running: false,
            finished: true,
            error: "image pull timed out after 120 s",
            lines: ["creating the data disk · done", "booting the VM · done", "installing Kivali 0.17.0 · failed"],
          },
        ),
      ]),
    preset: step3,
  },
  "connect": { hash: "#/connect", preset: { connect: { address: "https://dana-imac.local:8443" } } },
  "connect-checking": { hash: "#/connect", preset: { connect: { address: "https://dana-imac.local:8443", phase: "checking" } } },
  "connect-found": { hash: "#/connect", preset: { connect: { address: "https://dana-imac.local:8443", phase: "found", found: { origin: "https://dana-imac.local:8443", name: "Plainsong", host: "dana-imac.local" } } } },
  "connect-unreachable": { hash: "#/connect", preset: { connect: { address: "https://dana-imac.local:8443", phase: "error", error: { kind: "unreachable", message: "connection refused" } } } },
  "connect-not-kivali": { hash: "#/connect", preset: { connect: { address: "https://dana-imac.local", phase: "error", error: { kind: "not_kivali", message: "no /api/v1/login" } } } },
  "connect-not-invited": {
    hash: "#/connect",
    snap: (s) => (s.connect = { state: "not_invited", origin: "https://dana-imac.local:8443", name: "Plainsong", error: null, team_id: null, email: "sam.work@example.com" }),
    preset: { connect: { address: "https://dana-imac.local:8443", phase: "found", found: { origin: "https://dana-imac.local:8443", name: "Plainsong", host: "dana-imac.local" } } },
  },
  "team-paused": { hash: "#/team/home" },
  "team-waking": {
    hash: "#/team/home",
    snap: (s) => {
      const t = s.teams[1];
      Object.assign(t, { state: "starting", activity: "waking", phrase: "waking up", op: op({ kind: "resume", percent: 40, remaining: "about 30 seconds", stage: "Starting up" }) });
    },
  },
  "team-updating": {
    hash: "#/team/plainsong",
    snap: (s) => {
      const t = s.teams[0];
      Object.assign(t, {
        state: "starting",
        activity: "updating",
        phrase: "updating",
        version: "0.16.0",
        update: { state: "available", current: "0.16.0", latest: "0.17.0", notes_url: "https://kivali.ai/notes/0.17", summary: null, checked_at: iso(HOUR), takes: "about 3 minutes" },
        op: op({ kind: "update", stages: UPDATE_STAGES, stage: "Saving a snapshot", stage_index: 1, percent: 35, remaining: "about 3 minutes" }),
      });
    },
  },
  "team-failed": {
    hash: "#/team/plainsong",
    snap: (s) => {
      Object.assign(s.teams[0], {
        state: "failed",
        phrase: "couldn't start",
        pause_first: "home",
        op: op({ kind: "resume", running: false, finished: true, error: "not enough memory: need 4096 MB, 2048 MB free", lines: ["checking memory · 2048 MB free"] }),
      });
      Object.assign(s.teams[1], { state: "running", phrase: "running", paused_since: null });
    },
  },
  "team-unreachable": { hash: "#/team/studio", snap: (s) => Object.assign(s.teams[2], { state: "paused", phrase: "can't reach dana-imac.local" }) },
  "settings-general": { hash: "#/settings/general" },
  "settings-overview-update": {
    hash: "#/settings/team/plainsong/overview",
    snap: (s) =>
      Object.assign(s.teams[0], {
        version: "0.16.0",
        update: {
          state: "available",
          current: "0.16.0",
          latest: "0.17.0",
          notes_url: "https://kivali.ai/notes/0.17",
          summary: "new knowledge graph view, faster startup",
          checked_at: iso(HOUR),
          takes: "about 3 minutes",
        },
      }),
  },
  "settings-ai": { hash: "#/settings/team/plainsong/ai" },
  "settings-ai-signed-out": { hash: "#/settings/team/plainsong/ai", snap: (s) => Object.assign(s.teams[0], { phrase: "Claude isn’t signed in", ai: { ...signedOut, signed_out_at: onDay(10, 2) } }) },
  "settings-ai-bedrock": { hash: "#/settings/team/home/ai", snap: (s) => Object.assign(s.teams[1], { state: "running", phrase: "running", paused_since: null }) },
  "settings-mac": { hash: "#/settings/team/plainsong/mac", snap: (s) => Object.assign(s.teams[1], { state: "running", phrase: "running", paused_since: null }) },
  "settings-devices": { hash: "#/settings/team/plainsong/devices" },
  "settings-devices-address": { hash: "#/settings/team/plainsong/devices", preset: { devices: { team: "plainsong", address: "" } } },
  "settings-devices-error": {
    hash: "#/settings/team/plainsong/devices",
    preset: { devices: { team: "plainsong", address: "https://dana-imac.tailnet.ts.net", error: "Couldn’t reach https://dana-imac.tailnet.ts.net: connection refused." } },
  },
  "settings-devices-saved": { hash: "#/settings/team/plainsong/devices", snap: (s) => (s.teams[0].public_url = "https://dana-imac.tailnet.ts.net") },
  "settings-elsewhere": { hash: "#/settings/team/studio" },
  "settings-advanced": { hash: "#/settings/advanced" },
  "settings-deleted": {
    hash: "#/settings/general",
    snap: (s) => {
      s.teams = s.teams.filter((t) => t.id !== "plainsong");
      s.last_deleted = { name: "Plainsong", freed_bytes: 18 * 1024 ** 3 };
    },
  },
  "dialog-update": {
    hash: "#/settings/team/plainsong/overview",
    snap: (s) =>
      Object.assign(s.teams[0], {
        version: "0.16.0",
        update: { state: "available", current: "0.16.0", latest: "0.17.0", notes_url: "https://kivali.ai/notes/0.17", summary: null, checked_at: iso(HOUR), takes: "about 3 minutes" },
      }),
    preset: { dialog: { id: "update", team: "plainsong" } },
  },
  "dialog-memory": { hash: "#/team/home", preset: { dialog: { id: "memory", team: "home", other: "plainsong" } } },
  "dialog-remove": { hash: "#/settings/team/studio", preset: { dialog: { id: "remove", team: "studio" } } },
  "dialog-delete": { hash: "#/settings/team/plainsong/overview", preset: { dialog: { id: "delete", team: "plainsong" } } },
  "dialog-delete-confirm": { hash: "#/settings/team/plainsong/overview", preset: { dialog: { id: "delete-confirm", team: "plainsong", typed: "Plainsong" } } },
};

export function installMock() {
  const params = new URLSearchParams(location.search);
  const id = params.get("state") ?? "settings-general";
  const fx = FIXTURES[id] ?? FIXTURES["settings-general"];
  const snap = base();
  fx.snap?.(snap);
  if (!location.hash || location.hash === "#") history.replaceState(null, "", `${location.pathname}${location.search}${fx.hash}`);
  // Draw the page at its window's content size (window minus the drawn title bar).
  if (params.get("frame") !== "0") {
    const frame = () => {
      const app = document.getElementById("app")!;
      const page = location.hash.split("/")[1];
      const [w, hgt] = page === "settings" ? [880, 600] : page === "team" ? [1200, 720] : [720, 600];
      app.style.width = `${w}px`;
      app.style.height = `${hgt}px`;
      app.style.outline = "1px solid rgba(127,127,127,.4)";
      app.style.position = "relative";
      app.style.transform = "translateZ(0)";
    };
    frame();
    window.addEventListener("hashchange", frame);
  }
  const listeners: (() => void)[] = [];
  const changed = () => setTimeout(() => listeners.forEach((l) => l()), 0);
  const find = (tid: unknown) => snap.teams.find((t) => t.id === tid);
  const log = (cmd: string, args?: Record<string, unknown>) => console.info("[mock]", cmd, args ?? "");

  const handlers: Record<string, (a: Record<string, any>) => unknown> = {
    shell_snapshot: () => structuredClone(snap),
    set_title: (a) => {
      document.title = String(a.title);
      return null;
    },
    owner_signin_start: () => {
      snap.owner_signin = { state: "waiting", email: null, error: null };
      setTimeout(() => {
        snap.owner_signin = { state: "signed_in", email: "dana@example.com", error: null };
        changed();
      }, 1500);
      return null;
    },
    owner_signin_reset: () => ((snap.owner_signin = { state: "idle", email: null, error: null }), null),
    create_team: (a) => {
      const t = creating({ name: a.team.name, kind: a.team.kind, owner: a.team.owner });
      snap.teams = snap.teams.filter((x) => x.id !== t.id).concat(t);
      // Walk the stages.
      let i = 0;
      const tick = setInterval(() => {
        i += 1;
        const cur = find(t.id);
        if (!cur || !cur.op) return clearInterval(tick);
        if (i >= 8) {
          Object.assign(cur, { state: "running", activity: null, provisional: false });
          cur.op = { ...cur.op, running: false, finished: true, percent: 100, stage: "Ready", stage_index: 3 };
          clearInterval(tick);
        } else {
          const idx = Math.min(2, Math.floor(i / 3));
          cur.op = { ...cur.op, percent: i * 12, stage: CREATE_STAGES[idx], stage_index: idx, remaining: `about ${(8 - i) * 5} seconds`, lines: [...cur.op.lines, `step ${i}`] };
        }
        changed();
      }, 1000);
      return t.id;
    },
    open_claude_signin: (a) => {
      const t = find(a.id);
      if (!t || t.state !== "running") throw "The team isn't running yet.";
      t.ai = { ...t.ai!, terminal_open: true, signin: "waiting" };
      setTimeout(() => {
        t.ai = { ...t.ai!, terminal_open: false, signin: "idle", signed_in: true, email: "dana@example.com", billing: "Claude Max" };
        changed();
      }, 3000);
      return null;
    },
    credential_setup: (a) => {
      const t = find(a.id);
      const saved = t?.ai?.billing?.startsWith("Microsoft Foundry · ") ? t.ai.billing.slice("Microsoft Foundry · ".length) : null;
      return {
        models: ["claude-haiku-4-5", "claude-sonnet-5", "claude-opus-5-5", "claude-fable-5-1"],
        ...(saved ? { current: { setup: "microsoft-foundry", values: { resource: saved, auth: "api_key" } } } : {}),
      };
    },
    // A resource named "refused" is refused; one with "missing" in it lacks the Fable deployment.
    apply_credential_setup: async (a) => {
      await new Promise((r) => setTimeout(r, 1500));
      const t = find(a.id)!;
      const resource = String(a.values.resource);
      if (resource.includes(".")) throw "enter the resource name only, like my-resource, not a URL";
      const billing = `Microsoft Foundry · ${resource}`;
      t.ai = { ...t.ai!, signed_in: true, email: null, billing, signin: "idle", terminal_open: false };
      const models = ["claude-haiku-4-5", "claude-sonnet-5", "claude-opus-5-5", "claude-fable-5-1"].map((model) =>
        resource === "refused"
          ? { model, ok: false, problem: "the provider refused the sign-in (HTTP 401)" }
          : resource.includes("missing") && model === "claude-fable-5-1"
            ? { model, ok: false, missing: true, problem: `No deployment named ${model} in ${resource}` }
            : { model, ok: true },
      );
      return { credential: { signed_in: true, billing, checked_at: new Date().toISOString() }, models };
    },
    finish_setup: () => null,
    prepare_team: () => null,
    discard_prepared: () => null,
    abandon_team: (a) => ((snap.teams = snap.teams.filter((t) => t.id !== a.id)), null),
    retry_create: (a) => {
      snap.teams = snap.teams.filter((t) => t.id !== a.id);
      return (handlers.create_team as (x: Record<string, unknown>) => string)({ team: { name: "Plainsong", kind: "work", owner: "dana@example.com" } });
    },
    connect_check: (a) => {
      const addr = String(a.address);
      if (addr.includes("nope")) throw { kind: "unreachable", message: "connection refused" };
      if (addr.includes("example")) throw { kind: "not_kivali", message: "no login" };
      const host = addr.replace(/^https?:\/\//, "").replace(/[:/].*$/, "");
      return { origin: addr, name: "Plainsong", host };
    },
    connect_signin: (a) => {
      snap.connect = { state: "signing_in", origin: a.origin, name: a.name, error: null, team_id: null, email: null };
      setTimeout(() => {
        snap.connect = { state: "not_invited", origin: a.origin, name: a.name, error: null, team_id: null, email: "sam.work@example.com" };
        changed();
      }, 1500);
      return null;
    },
    connect_reset: () => ((snap.connect = { state: "idle", origin: null, name: null, error: null, team_id: null, email: null }), null),
    confirm_pause: (a) => (Object.assign(find(a.id)!, { state: "paused", phrase: "paused", paused_since: new Date().toISOString() }), null),
    resume_team: (a) => {
      const t = find(a.id)!;
      const other = snap.teams.find((x) => x.id !== t.id && x.place === "here" && x.state === "running");
      if (other && id === "team-paused") throw `memory:${other.id}`;
      Object.assign(t, { state: "running", phrase: "running", paused_since: null });
      return null;
    },
    pause_and_resume: (a) => {
      Object.assign(find(a.pause)!, { state: "paused", phrase: "paused", paused_since: new Date().toISOString() });
      Object.assign(find(a.resume)!, { state: "running", phrase: "running", paused_since: null, pause_first: null });
      return null;
    },
    team_facts: () => ({ agents: 6, files: 1204, disk_used_bytes: 18 * 1024 ** 3 }),
    delete_team: (a) => {
      const t = find(a.id)!;
      if (a.typed !== t.name) throw "The name doesn't match.";
      snap.teams = snap.teams.filter((x) => x.id !== t.id);
      snap.last_deleted = { name: t.name, freed_bytes: t.disk_used_bytes ?? 0 };
      return null;
    },
    remove_team: (a) => ((snap.teams = snap.teams.filter((x) => x.id !== a.id)), null),
    set_resources: (a) => (Object.assign(find(a.id)!, { memory_mb: a.memory_mb, cpus: a.cpus }), null),
    set_setting: (a) => (((snap.settings as Record<string, boolean>)[a.key] = a.value), null),
    check_app_update: () => {
      snap.app_update = { state: "checking", version: null, checked_at: null, error: null };
      setTimeout(() => {
        snap.app_update = { state: "up_to_date", version: null, checked_at: new Date().toISOString(), error: null };
        changed();
      }, 1200);
      return null;
    },
    save_diagnostics: () => "/Users/dana/Desktop/Kivali diagnostics.zip",
    set_public_url: async (a) => {
      await new Promise((r) => setTimeout(r, 800));
      const t = find(a.id)!;
      if (a.address === null) return (t.public_url = null);
      const addr = String(a.address);
      if (!addr.startsWith("https://")) throw "The address must start with https://.";
      if (addr.includes("nope")) throw `Couldn’t reach ${addr}: connection refused.`;
      return (t.public_url = addr.replace(/\/+$/, ""));
    },
  };

  const invoke = async <T>(cmd: string, args?: Record<string, unknown>): Promise<T> => {
    log(cmd, args);
    const fn = handlers[cmd];
    const out = fn ? await fn(args ?? {}) : null;
    if (cmd !== "shell_snapshot" && cmd !== "set_title") changed();
    return out as T;
  };
  const listen = async (event: string, cb: () => void) => {
    if (event === "shell-changed") listeners.push(cb);
    return () => undefined;
  };
  return { invoke, listen, preset: structuredClone(fx.preset ?? {}) as Preset };
}
