// Shapes for the logic tests (not imported by the pages).

import type { OpView, Snapshot, Team } from "../types";

export function op(o: Partial<OpView> = {}): OpView {
  return {
    kind: "create",
    running: true,
    error: null,
    finished: false,
    lines: [],
    stage: null,
    stage_index: 0,
    stages: [],
    started_ms: 0,
    percent: 0,
    remaining: null,
    ...o,
  };
}

export function team(o: Partial<Team> = {}): Team {
  return {
    id: "t1",
    name: "Plainsong",
    kind: "work",
    place: "here",
    url: null,
    device: null,
    state: "running",
    activity: null,
    phrase: "running",
    reason: null,
    pause_first: null,
    op: null,
    version: "0.17.0",
    update: null,
    ai: { provider: "claude", signed_in: true, email: "dana@example.com", billing: "Claude Max", terminal_open: false, signin: "idle", signed_out_at: null },
    owner: "dana@example.com",
    memory_mb: 4096,
    cpus: 4,
    disk_used_bytes: null,
    disk_size_bytes: null,
    paused_since: null,
    last_reached: null,
    provisional: false,
    public_url: null,
    agents: null,
    working: null,
    files: null,
    signed_in_as: null,
    memory_free_mb: null,
    ...o,
  };
}

export function snapshot(o: Partial<Snapshot> = {}): Snapshot {
  return {
    app_version: "0.17.1",
    platform: "macos",
    teams: [],
    host: { memory_mb: 16384, cpus: 8 },
    settings: { start_at_login: true, ask_before_quit: true },
    app_update: { state: "not_checked", version: null, checked_at: null, error: null },
    owner_signin: { state: "idle", email: null, error: null },
    connect: { state: "idle", origin: null, name: null, error: null, team_id: null, email: null },
    config_dir: "/x",
    logs_dir: "/y",
    last_deleted: null,
    ...o,
  };
}
