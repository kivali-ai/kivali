// The setup window's create path: the welcome (or "Add a team"), then four
// steps, and a failure screen when getting the team ready fails. The step and
// answers live in page memory; the team's progress comes from the
// snapshot. Decisions: logic/setup.ts.

import lockupDark from "../../../design-system/assets/logos/kivali-lockup-dark.svg";
import lockup from "../../../design-system/assets/logos/kivali-lockup.svg";
import type { Ctx } from "../ctx";
import { cx, h, type Child } from "../h";
import { icon, type IconName } from "../icons";
import { takePreset } from "../ipc";
import {
  backFrom,
  failureText,
  CALL_ME_HINT,
  NAME_HINT,
  needsCreate,
  newSetup,
  opensWhen,
  ownerSigninErrorText,
  readyBar,
  setupScreen,
  stepOf,
  type SetupScreen,
  type SetupState,
} from "../logic/setup";
import { thisComputer } from "../logic/teams";
import type { Kind, Team } from "../types";
import { banner, button, caption, disc, field, orgMark, personAvatar, progress, radioMark, readyDot } from "../ui";
import { foundryDialog, openFoundry } from "./foundry";

let s: SetupState = newSetup();
/** The team id last seen in a snapshot (see `render`). */
let seenTeam: string | null = null;
let presetRead = false;

function team(ctx: Ctx): Team | undefined {
  return s.teamId ? ctx.snap.teams.find((t) => t.id === s.teamId) : undefined;
}

export function render(ctx: Ctx): Node[] {
  if (!presetRead) {
    presetRead = true;
    const p = takePreset("setup");
    if (p) s = { ...s, ...p };
  }
  const r = ctx.route;
  if (r.page !== "setup" || r.mode !== "new") return [welcome(ctx, r.page === "setup" && r.mode === "add")];
  // A team that disappeared (abandoned elsewhere) takes setup back to step
  // 2: only one this page has seen in a snapshot. A snapshot from before
  // create_team returned does not list it yet, and forgetting it then made
  // every Continue create another team.
  if (s.teamId && team(ctx)) seenTeam = s.teamId;
  if (s.teamId && seenTeam === s.teamId && !team(ctx) && s.step >= 3) {
    s.teamId = null;
    s.created = null;
    s.step = 2;
  }
  const screen = setupScreen(s, ctx.snap);
  if ((screen === "done-work" || screen === "done-personal") && s.step !== 4) s.step = 4;
  const sheet = foundryDialog(ctx);
  return sheet ? [screenView(ctx, screen), sheet] : [screenView(ctx, screen)];
}

function welcomeRoute(ctx: Ctx): string {
  return ctx.snap.teams.length ? "#/setup/add" : "#/setup/welcome";
}

// ---- Layout ----

function frame(ctx: Ctx, body: Child[], foot: { step: number | null; buttons: Child[] } | null, bar: Child = null): HTMLElement {
  return h(
    "div",
    { class: "setup" },
    h("div", { class: "setup-body", id: "setup-body", "data-keep-scroll": true }, ...body, notice(ctx)),
    bar,
    foot
      ? h(
          "div",
          { class: "setup-foot" },
          h("span", { class: "mono-label" }, foot.step ? `Step ${foot.step} of 4` : ""),
          h("div", { class: "foot-actions" }, ...foot.buttons),
        )
      : null,
  );
}

function notice(ctx: Ctx): Child {
  return ctx.notice ? banner("danger", "Something went wrong", ctx.notice) : null;
}

function heading(title: string, lede?: string): HTMLElement {
  return h("div", { class: "heading" }, h("h1", { class: "h-setup" }, title), lede ? h("p", { class: "lede" }, lede) : null);
}

function choiceCard(o: {
  icon?: Node;
  title: Child[];
  desc: string;
  extra?: Child;
  trail: Node;
  selected?: boolean;
  disabled?: boolean;
  role?: string;
  id: string;
  onclick?: () => void;
}): HTMLElement {
  return h(
    "button",
    {
      type: "button",
      id: o.id,
      class: cx("choice", o.selected && "is-selected", o.disabled && "is-disabled"),
      role: o.role,
      "aria-checked": o.role ? String(!!o.selected) : null,
      disabled: o.disabled,
      onclick: o.onclick,
    },
    o.icon ? disc(o.icon) : null,
    h("span", { class: "choice-main" }, h("span", { class: "choice-title" }, ...o.title), h("span", { class: "choice-desc" }, o.desc), o.extra ?? null),
    o.trail,
  );
}

// ---- Welcome ----

function welcome(ctx: Ctx, add: boolean): HTMLElement {
  const go = (hash: string) => {
    if (hash === "#/connect" && s.teamId === null) void ctx.act("discard_prepared");
    if (hash === "#/setup/new") {
      if (s.step === 4 || s.teamId === null) s = newSetup();
      // The team's machine starts booting now; setup's questions overlap it.
      if (s.teamId === null) void ctx.act("prepare_team");
    }
    ctx.go(hash);
  };
  const card = (iconName: IconName, title: string, desc: string, hash: string, id: string) =>
    choiceCard({ id, icon: icon(iconName, 20), title: [title], desc, trail: h("span", { class: "choice-chev" }, icon("chevron-right", 20)), onclick: () => go(hash) });
  const body = [
    h(
      "span",
      { class: "lockup" },
      h("img", { src: lockup, alt: "Kivali", class: "lockup-light" }),
      h("img", { src: lockupDark, alt: "", class: "lockup-dark" }),
    ),
    heading(add ? "Add a team" : "Welcome to Kivali", `Create a team of agents on ${thisComputer(ctx.snap.platform)}, or connect to one you already run somewhere else.`),
    h(
      "div",
      { class: "stack-12" },
      card("laptop", "Create a team", `It runs on ${thisComputer(ctx.snap.platform)}. Your agents work while it’s awake.`, "#/setup/new", "pick-create"),
      card("globe", "Connect to a team", "Already running on another computer or a server? Connect to it with its address.", "#/connect", "pick-connect"),
    ),
  ];
  const foot = add ? { step: null, buttons: [button("Cancel", { variant: "ghost", id: "cancel", onclick: () => void ctx.act("close_window") })] } : null;
  return frame(ctx, body, foot);
}

// ---- The four steps ----

function screenView(ctx: Ctx, screen: SetupScreen): HTMLElement {
  const step = stepOf(screen);
  const t = team(ctx);
  const back = button("Back", { variant: "ghost", id: "back", onclick: () => void goBack(ctx) });
  // From step 3 on: the team starts getting ready when step 2 is done.
  const showBar = step !== null && step >= 3;
  const bar = showBar ? readyBarView(ctx, t) : null;
  switch (screen) {
    case "kind":
    case "kind-work":
    case "kind-personal":
      return frame(ctx, stepKind(ctx), { step, buttons: [back, continueButton(!canLeaveKind(), () => advance(ctx, 2))] });
    case "owner":
    case "owner-waiting":
    case "owner-signed-in":
    case "owner-failed":
      return frame(ctx, stepOwner(ctx, screen), {
        step,
        buttons: screen === "owner-signed-in" ? [back, continueButton(s.busy, () => void createTeam(ctx, 3), s.busy)] : [back],
      });
    case "account-preparing":
    case "account-ready":
    case "account-waiting":
    case "account-signed-in":
    case "account-closed":
      return frame(ctx, stepSignin(ctx, screen, t!), { step, buttons: [back, continueButton(screen !== "account-signed-in", () => advance(ctx, 4))] }, bar);
    case "done-work":
    case "done-personal":
      return frame(ctx, stepReady(ctx, t), {
        step,
        buttons: [button(`Open ${t?.name ?? s.name}`, { variant: "primary", id: "open-team", onclick: () => void finish(ctx) })],
      });
    case "create-failed":
      return frame(ctx, stepFailed(ctx, t!), {
        step: null,
        buttons: [
          button(s.detailsOpen ? "Hide details" : "Show details", {
            variant: "ghost",
            id: "details",
            onclick: () => {
              s.detailsOpen = !s.detailsOpen;
              ctx.rerender();
            },
          }),
          button("Try again", { variant: "primary", id: "retry", loading: s.busy, onclick: () => void retry(ctx) }),
        ],
      });
  }
}

function continueButton(disabled: boolean, onclick: () => void, loading = false): HTMLButtonElement {
  return button("Continue", { variant: "primary", id: "continue", disabled, loading, onclick });
}

function canLeaveKind(): boolean {
  return s.kind !== null && s.name.trim().length > 0;
}

function advance(ctx: Ctx, step: SetupState["step"]) {
  s.step = step;
  ctx.clearNotice();
  ctx.rerender();
  document.getElementById("setup-body")?.scrollTo(0, 0);
}

/** Removes the half-made team; false (with the reason shown) when it can't. */
async function abandon(ctx: Ctx, id: string): Promise<boolean> {
  try {
    await ctx.act("abandon_team", { id }, true);
    return true;
  } catch (e) {
    ctx.fail(e);
    return false;
  }
}

async function goBack(ctx: Ctx) {
  const b = backFrom(s);
  if (b.abandon && s.teamId) {
    // Stay put when the team can't be removed: carrying on would leave it
    // running with nothing in setup tracking it.
    if (!(await abandon(ctx, s.teamId))) return;
    s.teamId = null;
    s.created = null;
  }
  if (b.step === "welcome") {
    if (s.teamId === null) void ctx.act("discard_prepared");
    ctx.go(welcomeRoute(ctx));
    return;
  }
  advance(ctx, b.step);
}

// Step 1: what it's for, and its name.
function stepKind(ctx: Ctx): Child[] {
  const pick = (k: Kind) => {
    s.kind = k;
    ctx.rerender();
    queueMicrotask(() => document.getElementById("team-name")?.focus());
  };
  const kindCard = (k: Kind, iconName: IconName, title: string, desc: string, examples: string) =>
    choiceCard({
      id: `kind-${k}`,
      role: "radio",
      selected: s.kind === k,
      icon: icon(iconName, 20),
      title: [title],
      desc,
      extra: h("span", { class: "choice-examples" }, h("span", { class: "muted" }, "Agents like:"), ` ${examples}`),
      trail: radioMark(s.kind === k),
      onclick: () => pick(k),
    });
  const callField = s.kind
    ? field({
        id: "call-me",
        label: "What should your agents call you?",
        value: s.callMe,
        hint: CALL_ME_HINT[s.kind],
        autocomplete: "off",
        // The `> Name:` marker can't hold these; the shell drops them too.
        oninput: (v) => {
          const kept = v.replace(/[:`>]/g, "");
          s.callMe = kept;
          const input = document.getElementById("call-me") as HTMLInputElement | null;
          if (input && input.value !== kept) input.value = kept;
        },
        onenter: () => {
          if (canLeaveKind()) advance(ctx, 2);
        },
      }).wrap
    : null;
  const nameField = s.kind
    ? field({
        id: "team-name",
        label: "What should we call it?",
        value: s.name,
        hint: NAME_HINT[s.kind],
        autocomplete: "off",
        oninput: (v) => {
          s.name = v;
          const c = document.getElementById("continue") as HTMLButtonElement | null;
          if (c) c.disabled = !canLeaveKind();
        },
        onenter: () => {
          if (canLeaveKind()) advance(ctx, 2);
        },
      }).wrap
    : null;
  return [
    heading("What is this team for?"),
    h(
      "div",
      { class: "grid-2", role: "radiogroup", "aria-label": "What is this team for?" },
      kindCard("work", "building2", "Work", "For your job or your business. Run a company, a practice or a project with a team of agents.", "research assistant, software engineer, director of marketing"),
      kindCard("personal", "house", "Personal", "For your everyday life. Your home, plans, money, health, and the projects you do for yourself.", "travel agent, personal shopper, arborist"),
    ),
    nameField ?? caption("This sets the starting words and suggestions. You can change any of it later."),
    callField,
  ];
}

// Step 2: the owner's Google account.
function stepOwner(ctx: Ctx, screen: SetupScreen): Child[] {
  const name = s.name.trim();
  const signin = ctx.snap.owner_signin;
  const google = () =>
    h(
      "div",
      { class: "action-block" },
      button("Continue with Google", { variant: "primary", id: "google", onclick: () => void ctx.act("owner_signin_start") }),
      caption("Opens your browser. Kivali only reads your email address."),
    );
  const out: Child[] = [
    heading(`Sign in to make ${name} yours`, `${name} only lets your Google account in. You’ll use it to open ${name} here and from your other computers.`),
  ];
  if (screen === "owner") out.push(google());
  if (screen === "owner-waiting")
    out.push(
      statusCard(
        disc(icon("globe", 18)),
        "Finish signing in in your browser",
        "This updates when you’re done.",
        button("Open the browser again", { variant: "ghost", size: "sm", id: "browser-again", onclick: () => void ctx.act("owner_signin_start") }),
      ),
    );
  if (screen === "owner-signed-in")
    out.push(
      statusCard(
        personAvatar(signin.email ?? "", 40),
        `Signed in as ${signin.email ?? ""}`,
        `This account owns ${name}.`,
        button("Use a different account", { variant: "ghost", size: "sm", id: "other-account", onclick: () => void ctx.act("owner_signin_reset") }),
      ),
    );
  if (screen === "owner-failed") {
    out.push(banner("danger", "Couldn’t finish signing in", ownerSigninErrorText(signin.error)));
    out.push(google());
  }
  return out;
}

function statusCard(lead: Node, title: string, sub: string, action: Child = null): HTMLElement {
  return h(
    "div",
    { class: "status-card" },
    lead,
    h("div", { class: "status-card-main" }, h("span", { class: "strong" }, title), caption(sub)),
    action,
  );
}

/** Starts getting the team ready (Continue on step 2), then moves on to `next`. */
async function createTeam(ctx: Ctx, next: 3) {
  const owner = ctx.snap.owner_signin.email;
  if (!s.kind || !owner) return;
  const plan = needsCreate(s);
  if (plan === "reuse") return advance(ctx, next);
  s.busy = true;
  ctx.rerender();
  if (plan === "recreate" && s.teamId) {
    if (!(await abandon(ctx, s.teamId))) {
      s.busy = false;
      ctx.rerender();
      return;
    }
    s.teamId = null;
  }
  const answers = { name: s.name.trim(), kind: s.kind, callMe: s.callMe.trim() };
  const id = await ctx.act<string>("create_team", {
    team: { name: answers.name, kind: answers.kind, owner, call_me: answers.callMe },
  });
  s.busy = false;
  if (id) {
    s.teamId = id;
    s.created = answers;
    advance(ctx, next);
  } else ctx.rerender();
}

// Step 3: Claude Code's own sign-in, in Terminal.
function stepSignin(ctx: Ctx, screen: SetupScreen, t: Team): Child[] {
  const name = t.name;
  const open = () => void ctx.act("open_claude_signin", { id: t.id });
  const out: Child[] = [
    heading(
      "Connect Claude",
      "Claude Code’s sign-in opens in Terminal. Use a Claude subscription, an Anthropic Console account, Amazon Bedrock or Google Vertex AI. Kivali never sees what you type there.",
    ),
  ];
  const steps = () =>
    h(
      "ol",
      { class: "steps" },
      ...[
        `Terminal opens and starts Claude inside ${name}.`,
        "Choose how to sign in. Amazon Bedrock and Google Vertex AI are under 3rd-party platform.",
        "Follow Claude’s steps. For a subscription or Console account, open the link it shows and paste the code back into Terminal.",
      ].map((text, i) => h("li", {}, h("span", { class: "step-num" }, String(i + 1)), h("span", { class: "step-text" }, text))),
    );
  const foundry = () =>
    statusCard(
      disc(icon("key", 18)),
      "Using Microsoft Foundry?",
      "Claude’s sign-in in Terminal doesn’t set it up. Kivali does, here.",
      button("Use Microsoft Foundry…", { variant: "ghost", size: "sm", id: "use-foundry", onclick: () => openFoundry(ctx, t) }),
    );
  switch (screen) {
    case "account-preparing":
      out.push(steps(), h("div", { class: "action-block" }, button("Open Claude sign-in", { variant: "primary", id: "open-signin", disabled: true }), caption(opensWhen(t, name))));
      break;
    case "account-ready":
      out.push(steps(), h("div", { class: "action-block" }, button("Open Claude sign-in", { variant: "primary", id: "open-signin", onclick: open }), caption("Opens Terminal.")), foundry());
      break;
    case "account-waiting":
      out.push(
        steps(),
        statusCard(
          disc(icon("terminal", 18)),
          "Finish signing in in Terminal",
          "This updates when you’re done.",
          button("Open Terminal again", { variant: "ghost", size: "sm", id: "terminal-again", onclick: open }),
        ),
      );
      break;
    case "account-signed-in":
      out.push(statusCard(disc(icon("check", 18), "success"), "Signed in to Claude", signedInLine(t)));
      break;
    case "account-closed":
      out.push(
        banner("warning", "Not signed in yet", "Terminal closed before Claude finished signing in."),
        h("div", { class: "action-block" }, button("Open Claude sign-in again", { variant: "primary", id: "open-signin", onclick: open }), caption("Opens Terminal.")),
        foundry(),
      );
      break;
  }
  return out;
}

/** "Plainsong’s agents use Claude Max · dana@example.com." */
function signedInLine(t: Team): string {
  const how = [t.ai?.billing, t.ai?.email].filter((p): p is string => !!p).join(" · ");
  return how ? `${t.name}’s agents use ${how}.` : `${t.name}’s agents use this sign-in.`;
}

// Step 4.
function stepReady(ctx: Ctx, t: Team | undefined): Child[] {
  const name = t?.name ?? s.name.trim();
  const kind = t?.kind ?? s.kind;
  const owner = t?.owner ?? ctx.snap.owner_signin.email ?? "";
  const billing = t?.ai?.billing;
  const kindWord = kind === "personal" ? "Personal" : "Work";
  const line = (lead: Node, label: string, value: string) =>
    h("div", { class: "summary-row" }, h("span", { class: "summary-lead" }, lead), h("span", { class: "summary-label" }, label), h("span", { class: "muted" }, value));
  return [
    disc(icon("check", 20), "success"),
    heading(
      `${name} is ready`,
      kind === "personal" ? "Next, meet your chief of staff. They’ll ask a few questions to get to know you." : "Next, add some files and hire your chief of staff.",
    ),
    h(
      "div",
      { class: "box" },
      line(orgMark(name, 28), name, `${kindWord} · on ${thisComputer(ctx.snap.platform)}`),
      line(personAvatar(owner, 28), "Owner", owner),
      line(disc(icon("key", 16)), "AI", billing ? `Claude · ${billing}` : "Claude"),
    ),
  ];
}

async function finish(ctx: Ctx) {
  if (!s.teamId) return;
  const id = s.teamId;
  const ok = await ctx.act("finish_setup", { id });
  if (ok !== undefined) s = newSetup();
}

// A20.
function stepFailed(ctx: Ctx, t: Team): Child[] {
  return [
    disc(icon("triangle-alert", 20)),
    heading(`${t.name} couldn’t be set up`, `Nothing was changed on ${thisComputer(ctx.snap.platform)}. Trying again usually works; if it doesn’t, the details help us find out why.`),
    h("pre", { class: "lines", id: "fail-lines", "data-keep-scroll": true }, failureText(t.op!, s.detailsOpen)),
  ];
}

async function retry(ctx: Ctx) {
  if (!s.teamId) return;
  s.busy = true;
  ctx.rerender();
  const id = await ctx.act<string>("retry_create", { id: s.teamId });
  s.busy = false;
  if (id) {
    s.teamId = id;
    s.detailsOpen = false;
    s.step = 3;
  }
  ctx.rerender();
}

// The getting-ready bar: click to show the raw lines.
function readyBarView(ctx: Ctx, t: Team | undefined): Child {
  const b = readyBar(t);
  if (!b) return null;
  const toggle = () => {
    s.barOpen = !s.barOpen;
    ctx.rerender();
  };
  const head =
    b.state === "ready"
      ? h("span", { class: "bar-title" }, readyDot(), h("strong", {}, b.title))
      : h("span", { class: "bar-title" }, h("strong", {}, b.title), b.stage ? h("span", { class: "muted" }, `· ${b.stage}`) : null);
  const lines = t?.op?.lines ?? [];
  return h(
    "div",
    { class: cx("ready-bar", s.barOpen && "is-open") },
    h(
      "button",
      { type: "button", class: "ready-bar-toggle", id: "ready-bar", "aria-expanded": String(s.barOpen), "aria-controls": "ready-lines", onclick: toggle },
      // One compact row: what is happening, a thin bar, the time left.
      head,
      b.state === "working" ? h("span", { class: "ready-bar-meter" }, progress(b.percent, "ink", b.title)) : null,
      b.remaining ? h("span", { class: "muted ready-bar-left" }, b.remaining) : null,
    ),
    s.barOpen
      ? h("pre", { class: "lines lines--bar", id: "ready-lines", "data-keep-scroll": true }, lines.length ? lines.join("\n") : "No details yet.")
      : null,
  );
}

