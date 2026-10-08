// The Settings window: General, one page per team with tabs
// Overview / AI / This Mac / Other devices or the connection for a team
// elsewhere, and Advanced. Dialogs: dialogs.ts.

import type { Ctx } from "../ctx";
import { cx, h, type Child } from "../h";
import { icon } from "../icons";
import { takePreset } from "../ipc";
import { ago, deviceName, deviceShort, formatBytes, formatMemory, normalizeAddress, tildePath } from "../logic/format";
import { routeHash, type TeamTab } from "../logic/route";
import { errorText } from "../logic/setup";
import { claudeLine, cpuOptions, devicesHint, devicesView, diskLines, memoryConflict, signedOutLine, signInLine, memoryOptions, memoryUse, placeLine, statusRow, thisComputer, updateRow } from "../logic/teams";
import type { DialogId } from "../preset";
import type { Snapshot, Team } from "../types";
import { banner, button, caption, card, dot, field, monoLabel, orgMark, row, select, settingsButtonLabel, switchRow } from "../ui";
import { confirmDeleteSheet, deleteSheet, memorySheet, pauseSheet, removeSheet, updateSheet, type Facts } from "./dialogs";
import { foundryDialog, openFoundry } from "./foundry";
import { isFoundry } from "../logic/foundry";

let dialog: { id: DialogId; team: string; other?: string } | null = null;
let e8 = { typed: "", busy: false, error: null as string | null };
let facts: Facts | null = null;
let deletedSeen: string | null = null;
let savedTo: string | null = null;
let presetRead = false;
/** Other devices, per team: the switch's local state and the address field. */
const devices: Record<string, { revealed: boolean; editing: boolean; address: string; busy: boolean; error: string | null; copied: boolean }> = {};

function devState(id: string) {
  return (devices[id] ??= { revealed: false, editing: false, address: "", busy: false, error: null, copied: false });
}

function openDialog(ctx: Ctx, id: DialogId, team: string, other?: string) {
  dialog = { id, team, other };
  if (id === "delete-confirm") e8 = { typed: "", busy: false, error: null };
  if (id === "delete") loadFacts(ctx, team);
  ctx.rerender();
}

function loadFacts(ctx: Ctx, team: string) {
  facts = null;
  void ctx.act<Facts>("team_facts", { id: team }).then((f) => {
    if (f && dialog?.id === "delete") {
      facts = f;
      ctx.rerender();
    }
  });
}

function closeDialog(ctx: Ctx) {
  dialog = null;
  ctx.rerender();
}

export function render(ctx: Ctx): Node[] {
  if (!presetRead) {
    presetRead = true;
    const p = takePreset("dialog");
    if (p) {
      dialog = { id: p.id, team: p.team, other: p.other };
      if (p.typed) e8.typed = p.typed;
      if (p.id === "delete") loadFacts(ctx, p.team);
    }
    const dv = takePreset("devices");
    if (dv) Object.assign(devState(dv.team), { revealed: true, address: dv.address, error: dv.error ?? null });
  }
  const r = ctx.route;
  if (r.page !== "settings") return [];
  const s = ctx.snap;
  let content: Child[] = general(ctx);
  if (r.section === "advanced") content = advanced(ctx);
  else if (r.section === "team") {
    const t = s.teams.find((x) => x.id === r.id);
    content = t ? (t.place === "elsewhere" ? elsewhere(ctx, t) : teamPage(ctx, t, r.tab)) : general(ctx);
  }
  if (r.section !== "general" && s.last_deleted) deletedSeen = s.last_deleted.name;
  if (ctx.notice) content.unshift(banner("danger", "Something went wrong", ctx.notice));
  const out: Node[] = [
    h("div", { class: "settings" }, sidebar(ctx), h("main", { class: "settings-main", id: "settings-main", "data-keep-scroll": true }, ...content)),
  ];
  const d = dialogView(ctx) ?? foundryDialog(ctx);
  if (d) out.push(d);
  return out;
}

// ---- Sidebar ----

function sidebar(ctx: Ctx): HTMLElement {
  const r = ctx.route;
  const cur = r.page === "settings" ? (r.section === "team" ? `team:${r.id}` : r.section) : "";
  const nav = (key: string, hash: string, lead: Node, label: string, trail: Child = null, onclick?: () => void) =>
    h(
      "a",
      {
        class: cx("kv-nav", cur === key && "is-active"),
        href: hash,
        "aria-current": cur === key ? "page" : null,
        onclick: onclick
          ? (e: Event) => {
              e.preventDefault();
              onclick();
            }
          : null,
      },
      lead,
      h("span", { class: "kv-nav-label" }, label),
      trail,
    );
  const teams = ctx.snap.teams.map((t) =>
    nav(
      `team:${t.id}`,
      routeHash({ page: "settings", section: "team", id: t.id, tab: "overview" }),
      orgMark(t.name, 20),
      t.name,
      t.place === "elsewhere" ? h("span", { class: "caption muted" }, deviceShort(t.device)) : h("span", { class: "nav-dot", title: t.phrase }, dot(t.state, 8), h("span", { class: "sr-only" }, t.phrase)),
    ),
  );
  return h(
    "nav",
    { class: "settings-side", "aria-label": "Settings" },
    nav("general", "#/settings/general", icon("settings", 20), "General"),
    h("span", { class: "mono-label side-label" }, "Teams"),
    ...teams,
    nav("add", "#", icon("plus", 20), "Add a team", null, () => void ctx.act("open_setup", { route: "add" })),
    h("div", { class: "side-gap" }),
    nav("advanced", "#/settings/advanced", icon("wrench", 20), "Advanced"),
  );
}

// ---- General ----

function general(ctx: Ctx): Child[] {
  const s = ctx.snap;
  const out: Child[] = [];
  if (s.last_deleted && deletedSeen !== s.last_deleted.name) {
    out.push(banner("success", `${s.last_deleted.name} was deleted · ${formatBytes(s.last_deleted.freed_bytes)} freed`));
  }
  out.push(
    h("h1", { class: "h-page" }, "General"),
    h(
      "div",
      { class: "stack-14" },
      switchRow({
        id: "start-at-login",
        label: "Open Kivali at login",
        hint: "Teams that were running when you quit resume too.",
        checked: s.settings.start_at_login,
        onchange: (v) => void ctx.act("set_setting", { key: "start_at_login", value: v }),
      }),
      switchRow({
        id: "ask-before-quit",
        label: "Ask before quitting",
        hint: `Quitting pauses every team on ${thisComputer(s.platform)}.`,
        checked: s.settings.ask_before_quit,
        onchange: (v) => void ctx.act("set_setting", { key: "ask_before_quit", value: v }),
      }),
    ),
  );
  out.push(appUpdate(ctx));
  return out;
}

function appUpdate(ctx: Ctx): HTMLElement {
  const s = ctx.snap;
  const u = s.app_update;
  const checked = u.checked_at ? ago(u.checked_at, ctx.now) : null;
  let sub: string;
  let action: Child = button("Check for updates", { variant: "secondary", size: "sm", id: "app-check", onclick: () => void ctx.act("check_app_update") });
  switch (u.state) {
    case "checking":
      sub = "Checking for updates…";
      action = button("Check for updates", { variant: "secondary", size: "sm", id: "app-check", loading: true });
      break;
    case "up_to_date":
      sub = checked ? `Up to date · checked ${checked}` : "Up to date";
      break;
    case "available":
      sub = `${u.version} is available`;
      action = button("Update…", { variant: "primary", size: "sm", id: "app-install", onclick: () => void ctx.act("install_app_update") });
      break;
    case "installing":
      sub = `Installing ${u.version ?? "the update"}…`;
      action = null;
      break;
    case "failed":
      sub = u.error ? `Couldn’t check for updates · ${u.error}` : "Couldn’t check for updates";
      break;
    default:
      sub = "Not checked yet";
  }
  return h(
    "section",
    { class: "stack-8" },
    monoLabel("Kivali"),
    card(row({ title: `Kivali ${s.app_version}`, sub, trail: action, cls: u.state === "available" ? "is-highlight" : undefined })),
  );
}

// ---- Advanced ----

function advanced(ctx: Ctx): Child[] {
  const s = ctx.snap;
  const show = settingsButtonLabel(s.platform);
  return [
    h("h1", { class: "h-page" }, "Advanced"),
    card(
      row({
        title: "Logs",
        sub: "What Kivali and each team’s machine did, newest first.",
        trail: button(show, { variant: "secondary", size: "sm", id: "open-logs", onclick: () => void ctx.act("open_logs") }),
      }),
      row({
        title: "Settings folder",
        sub: h("span", { class: "caption muted" }, h("span", { class: "mono-path" }, tildePath(s.config_dir))),
        trail: button(show, { variant: "secondary", size: "sm", id: "open-config", onclick: () => void ctx.act("open_config_dir") }),
      }),
      row({
        title: "Diagnostics",
        sub: [
          "Logs and settings in one file for support. No team data, no sign-ins.",
          savedTo ? h("span", { class: "caption muted" }, "Saved to ", h("span", { class: "mono-path" }, tildePath(savedTo))) : null,
        ],
        trail: button("Save…", {
          variant: "secondary",
          size: "sm",
          id: "save-diag",
          onclick: async () => {
            const path = await ctx.act<string>("save_diagnostics");
            if (path && path !== "cancelled") {
              savedTo = path;
              ctx.rerender();
            }
          },
        }),
      }),
    ),
  ];
}

// ---- A team here: Overview, AI, This Mac ----

function header(ctx: Ctx, t: Team): HTMLElement {
  return h(
    "div",
    { class: "team-head" },
    orgMark(t.name, 44, t.place === "elsewhere"),
    h("div", { class: "team-head-text" }, h("h1", { class: "h-team" }, t.name), caption(placeLine(t, ctx.snap.platform))),
  );
}

const TABS: { id: TeamTab; label: (s: Snapshot) => string }[] = [
  { id: "overview", label: () => "Overview" },
  { id: "ai", label: () => "AI" },
  { id: "mac", label: (s) => (s.platform === "macos" ? "This Mac" : s.platform === "windows" ? "This PC" : "This computer") },
  { id: "devices", label: () => "Other devices" },
];

function tabs(ctx: Ctx, t: Team, active: TeamTab): HTMLElement {
  const list = h("div", { class: "kv-tabs-list", role: "tablist", "aria-label": t.name });
  for (const tab of TABS) {
    const on = tab.id === active;
    list.appendChild(
      h(
        "button",
        {
          type: "button",
          role: "tab",
          class: "kv-tab",
          id: `tab-${tab.id}`,
          "aria-selected": String(on),
          "aria-controls": "tab-panel",
          tabindex: on ? "0" : "-1",
          "data-state": on ? "active" : "inactive",
          onclick: () => ctx.go(routeHash({ page: "settings", section: "team", id: t.id, tab: tab.id })),
        },
        tab.label(ctx.snap),
      ),
    );
  }
  list.addEventListener("keydown", (e) => {
    if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
    const i = TABS.findIndex((x) => x.id === active);
    const next = TABS[(i + (e.key === "ArrowRight" ? 1 : TABS.length - 1)) % TABS.length];
    ctx.go(routeHash({ page: "settings", section: "team", id: t.id, tab: next.id }));
    queueMicrotask(() => document.getElementById(`tab-${next.id}`)?.focus());
  });
  return list;
}

function teamPage(ctx: Ctx, t: Team, tab: TeamTab): Child[] {
  const panel = tab === "ai" ? aiTab(ctx, t) : tab === "mac" ? macTab(ctx, t) : tab === "devices" ? devicesTab(ctx, t) : overview(ctx, t);
  return [header(ctx, t), tabs(ctx, t, tab), h("div", { class: "tab-panel", role: "tabpanel", id: "tab-panel", "aria-labelledby": `tab-${tab}` }, ...panel)];
}

async function resume(ctx: Ctx, id: string) {
  try {
    await ctx.act("resume_team", { id }, true);
  } catch (e) {
    const other = memoryConflict(e);
    if (other) openDialog(ctx, "memory", id, other);
    else ctx.fail(e);
  }
}

function overview(ctx: Ctx, t: Team): Child[] {
  const st = statusRow(t, ctx.snap, ctx.now);
  let action: Child = null;
  if (st.action === "pause") action = button(st.actionLabel!, { variant: "secondary", size: "sm", id: "pause", onclick: () => openDialog(ctx, "pause", t.id) });
  if (st.action === "resume") action = button(st.actionLabel!, { variant: "secondary", size: "sm", id: "resume", onclick: () => void resume(ctx, t.id) });
  if (st.action === "retry")
    action = button(st.actionLabel!, {
      variant: "secondary",
      size: "sm",
      id: "retry",
      onclick: () => void (t.pause_first ? ctx.act("pause_and_resume", { pause: t.pause_first, resume: t.id }) : ctx.act("retry_team", { id: t.id })),
    });
  const statusLine = row({ lead: dot(t.state, 10), title: st.word, sub: st.sub, trail: action });
  const u = updateRow(t, ctx.now);
  const updateLine = u
    ? row({
        title: u.title,
        sub: u.sub,
        cls: u.highlight ? "is-highlight" : undefined,
        trail:
          u.notes || u.update
            ? h(
                "span",
                { class: "row-actions" },
                u.notes ? button("What’s new", { variant: "ghost", size: "sm", id: "whats-new", onclick: () => void ctx.act("open_external", { url: u.notes }) }) : null,
                u.update ? button("Update…", { variant: "primary", size: "sm", id: "update", onclick: () => openDialog(ctx, "update", t.id) }) : null,
              )
            : null,
      })
    : null;
  return [
    h("section", { class: "stack-8" }, monoLabel("Status"), card(statusLine, updateLine)),
    card(
      row({
        title: "Open in browser",
        sub: `${t.name} also works in any browser on ${thisComputer(ctx.snap.platform)}.`,
        trail: button("Open", { variant: "secondary", size: "sm", id: "open-browser", disabled: t.state !== "running", onclick: () => void ctx.act("open_in_browser", { id: t.id }) }),
      }),
    ),
    h(
      "div",
      { class: "danger-zone" },
      h("div", { class: "srow-main" }, h("span", { class: "srow-title" }, `Delete ${t.name}`), caption(`Removes its agents, files and chats from ${thisComputer(ctx.snap.platform)}.`)),
      button(`Delete ${t.name}…`, { variant: "danger", size: "sm", id: "delete", onclick: () => openDialog(ctx, "delete", t.id) }),
    ),
  ];
}

function aiTab(ctx: Ctx, t: Team): Child[] {
  const ai = t.ai;
  const provider = h("section", { class: "stack-8" }, monoLabel("Provider"), card(row({ title: "Claude", sub: "by Anthropic" })));
  const signIn = () => void ctx.act("open_claude_signin", { id: t.id });
  const waiting = ai?.signin === "waiting" ? "Finish signing in in Terminal. This updates when you’re done." : null;
  if (!ai || ai.signed_in === null) {
    return [provider, h("section", { class: "stack-8" }, monoLabel("How agents sign in"), caption(`Shows once ${t.name} is running.`))];
  }
  const foundry = isFoundry(ai.billing);
  const foundryRow = row({
    title: foundry ? "Microsoft Foundry" : "Use Microsoft Foundry",
    sub: foundry ? "Change the resource or how Kivali signs in to Azure." : "Bill Claude’s calls to your own Azure resource.",
    trail: button(foundry ? "Change…" : "Set up…", { variant: "secondary", size: "sm", id: "foundry", onclick: () => openFoundry(ctx, t) }),
  });
  if (ai.signed_in === false) {
    return [
      banner("warning", `${t.name}’s agents can’t work until Claude is signed in`, signedOutLine(t, ctx.now)),
      h(
        "section",
        { class: "stack-8" },
        monoLabel("How agents sign in"),
        card(
          row({ title: "Claude Code", sub: "A Claude subscription, an Anthropic Console account, Amazon Bedrock or Google Vertex AI", trail: h("span", { class: "caption attention" }, "Not signed in") }),
          row({
            title: "Sign in to Claude",
            sub: waiting ?? "Opens Claude’s sign-in in Terminal.",
            trail: button("Sign in…", { variant: "primary", size: "sm", id: "sign-in", onclick: signIn }),
          }),
          foundryRow,
        ),
      ),
    ];
  }
  return [
    provider,
    h(
      "section",
      { class: "stack-8" },
      monoLabel("How agents sign in"),
      card(
        row({ title: "Signed in to Claude", sub: claudeLine(t), trail: h("span", { class: "caption success-line" }, icon("circle-check", 16), "Signed in") }),
        foundry ? foundryRow : null,
        row({
          title: "Sign in again",
          sub:
            waiting ??
            (foundry
              ? "Opens Claude in Terminal to sign in another way. This removes the Microsoft Foundry settings."
              : "Sign in again to switch account or billing. Opens Claude in Terminal, where /login changes the sign-in."),
          trail: button("Sign in again…", { variant: "secondary", size: "sm", id: "sign-in-again", onclick: signIn }),
        }),
        foundry ? null : foundryRow,
      ),
    ),
  ];
}

function macTab(ctx: Ctx, t: Team): Child[] {
  const s = ctx.snap;
  const use = memoryUse(t, s);
  const bar = h("div", { class: "mem-bar", role: "img", "aria-label": use.label });
  const legend = h("div", { class: "mem-legend caption muted" });
  for (const seg of use.segments) {
    const el = h("span", { class: cx("mem-seg", seg.self ? "is-self" : "is-other") });
    el.style.width = `${seg.pct}%`;
    bar.appendChild(el);
    legend.appendChild(h("span", { class: "mem-key" }, h("span", { class: cx("mem-swatch", seg.self ? "is-self" : "is-other") }), `${seg.name} ${formatMemory(seg.mb)}`));
  }
  legend.appendChild(h("span", { class: "mem-key" }, h("span", { class: "mem-swatch is-free" }), `Free ${formatMemory(use.freeMb)}`));
  const setRes = (memory_mb: number, cpus: number) => void ctx.act("set_resources", { id: t.id, memory_mb, cpus });
  const disk = diskLines(t);
  return [
    h("section", { class: "stack-8" }, monoLabel(use.label), h("div", { class: "stack-8" }, bar, legend)),
    h(
      "div",
      { class: "grid-2 grid-16" },
      select({
        id: "memory",
        label: `Memory for ${t.name}`,
        options: memoryOptions(s.host.memory_mb, t.memory_mb),
        value: t.memory_mb,
        onchange: (v) => setRes(Number(v), t.cpus),
      }),
      select({ id: "cpus", label: "CPUs", options: cpuOptions(s.host.cpus, t.cpus), value: t.cpus, onchange: (v) => setRes(t.memory_mb, Number(v)) }),
    ),
    caption(`Changes apply the next time ${t.name} resumes.`),
    h("section", { class: "stack-8" }, monoLabel("Disk"), card(row({ title: disk.title, sub: disk.sub }))),
  ];
}

// ---- Other devices ----

function devicesTab(ctx: Ctx, t: Team): Child[] {
  const d = devState(t.id);
  const view = devicesView(t, d);
  const out: Child[] = [
    switchRow({
      id: "devices-switch",
      label: `Let other computers connect to ${t.name}`,
      hint: `Give ${t.name} an https address other computers can reach. Connecting still needs your Google sign-in.`,
      checked: view !== "off",
      onchange: (v) => {
        Object.assign(d, { revealed: v, editing: false, address: "", error: null, busy: false });
        if (!v && t.public_url) {
          void ctx.act("set_public_url", { id: t.id, address: null });
          return;
        }
        ctx.rerender();
        if (v) queueMicrotask(() => document.getElementById("public-address")?.focus());
      },
    }),
  ];
  if (view === "form") {
    const [before, cmd, after] = devicesHint(t);
    const f = field({
      id: "public-address",
      label: "Address",
      type: "url",
      inputmode: "url",
      placeholder: "https://dana-imac.tailnet.ts.net",
      value: d.address,
      hint: h("span", {}, before, h("span", { class: "mono-path" }, cmd), after),
      error: d.error,
      oninput: (v) => {
        d.address = v;
        const b = document.getElementById("save-address") as HTMLButtonElement | null;
        if (b && !d.busy) b.disabled = !v.trim();
      },
      onenter: () => void saveAddress(ctx, t),
    });
    out.push(
      f.wrap,
      h(
        "div",
        { class: "action-row" },
        button("Check and save", {
          variant: "primary",
          id: "save-address",
          loading: d.busy,
          disabled: !d.address.trim(),
          onclick: () => void saveAddress(ctx, t),
        }),
        d.editing
          ? button("Cancel", {
              variant: "ghost",
              id: "cancel-address",
              onclick: () => {
                Object.assign(d, { editing: false, error: null, address: "" });
                ctx.rerender();
              },
            })
          : null,
      ),
    );
  }
  if (view === "on" && t.public_url) {
    const url = t.public_url;
    out.push(
      h(
        "section",
        { class: "stack-8" },
        monoLabel("Address"),
        card(
          row({
            title: h("span", { class: "mono-address" }, url),
            sub: "On the other computer: File → Connect to a team, then paste this.",
            trail: h(
              "span",
              { class: "row-actions" },
              button("Change", {
                variant: "ghost",
                size: "sm",
                id: "change-address",
                onclick: () => {
                  Object.assign(d, { editing: true, address: url, error: null });
                  ctx.rerender();
                  queueMicrotask(() => document.getElementById("public-address")?.focus());
                },
              }),
              button(d.copied ? "Copied" : "Copy", {
                variant: "secondary",
                size: "sm",
                id: "copy-address",
                onclick: async () => {
                  try {
                    await navigator.clipboard.writeText(url);
                    d.copied = true;
                    ctx.rerender();
                    setTimeout(() => {
                      d.copied = false;
                      ctx.rerender();
                    }, 1500);
                  } catch (e) {
                    ctx.fail(e);
                  }
                },
              }),
            ),
          }),
        ),
      ),
      h(
        "section",
        { class: "stack-8" },
        monoLabel("Who can sign in"),
        card(
          row({
            title: signInLine(t),
            sub: "On the other computer, sign in with the same Google account.",
          }),
        ),
      ),
      caption(`Works while ${thisComputer(ctx.snap.platform)} is awake, ${t.name} is running, and the address you set up is serving.`),
    );
  }
  return out;
}

async function saveAddress(ctx: Ctx, t: Team) {
  const d = devState(t.id);
  if (!d.address.trim() || d.busy) return;
  Object.assign(d, { busy: true, error: null });
  ctx.rerender();
  try {
    await ctx.act("set_public_url", { id: t.id, address: normalizeAddress(d.address) }, true);
    Object.assign(d, { busy: false, editing: false, revealed: false, address: "" });
  } catch (e) {
    Object.assign(d, { busy: false, error: errorText(e) });
  }
  ctx.rerender();
}

// ---- A team elsewhere ----

function elsewhere(ctx: Ctx, t: Team): Child[] {
  const where = deviceName(t.device);
  const connected = t.state === "running";
  const sub = connected
    ? [t.url, t.version ? `Kivali ${t.version.replace(/^v/, "")}` : null].filter(Boolean).join(" · ")
    : t.last_reached
      ? `Last reached ${ago(t.last_reached, ctx.now)}`
      : "It may be asleep or offline.";
  return [
    header(ctx, t),
    h(
      "section",
      { class: "stack-8" },
      monoLabel("Connection"),
      card(
        row({
          lead: dot(connected ? "running" : "paused", 10),
          title: connected ? "Connected" : `Can’t reach ${where}`,
          sub,
          trail: connected ? null : button("Try again", { variant: "secondary", size: "sm", id: "retry", onclick: () => void ctx.act("retry_team", { id: t.id }) }),
        }),
        row({
          title: t.signed_in_as ? `Signed in as ${t.signed_in_as}` : "Signed in",
          trail: button("Sign out", { variant: "ghost", size: "sm", id: "sign-out", onclick: () => void ctx.act("sign_out_team", { id: t.id }) }),
        }),
      ),
    ),
    caption(`Pausing, updating and deleting ${t.name} happen on ${where}.`),
    h(
      "div",
      { class: "danger-zone" },
      h("div", { class: "srow-main" }, h("span", { class: "srow-title" }, `Remove from ${thisComputer(ctx.snap.platform)}`), caption(`${t.name} keeps running on ${where}.`)),
      button("Remove…", { variant: "secondary", size: "sm", id: "remove", onclick: () => openDialog(ctx, "remove", t.id) }),
    ),
  ];
}

// ---- Dialogs ----

function dialogView(ctx: Ctx): HTMLElement | null {
  if (!dialog) return null;
  const t = ctx.snap.teams.find((x) => x.id === dialog!.team);
  if (!t) {
    dialog = null;
    return null;
  }
  const close = () => closeDialog(ctx);
  switch (dialog.id) {
    case "pause":
      return pauseSheet(ctx, t, close);
    case "update":
      return updateSheet(ctx, t, close);
    case "memory":
      return memorySheet(ctx, t, ctx.snap.teams.find((x) => x.id === dialog!.other), close);
    case "remove":
      return removeSheet(ctx, t, close, () => ctx.go("#/settings/general"));
    case "delete":
      return deleteSheet(ctx, t, facts, () => openDialog(ctx, "delete-confirm", t.id), close);
    case "delete-confirm":
      return confirmDeleteSheet(
        ctx,
        t,
        e8,
        (v) => {
          e8.typed = v;
          e8.error = null;
        },
        () => void deleteTeam(ctx, t),
        close,
      );
  }
}

async function deleteTeam(ctx: Ctx, t: Team) {
  if (e8.typed !== t.name || e8.busy) return;
  e8.busy = true;
  ctx.rerender();
  try {
    await ctx.act("delete_team", { id: t.id, typed: e8.typed }, true);
    dialog = null;
    deletedSeen = null;
    e8.busy = false;
    ctx.go("#/settings/general");
  } catch (e) {
    e8.busy = false;
    const msg = errorText(e);
    e8.error = msg === "cancelled" ? null : msg;
    ctx.rerender();
  }
}

