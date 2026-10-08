// The setup window's connect path: an address, then the team's
// own Google sign-in. The check's answer lives in page memory; sign-in
// progress comes from snapshot.connect. On done the shell closes this
// window and opens the team's.

import type { Ctx } from "../ctx";
import { h, type Child } from "../h";
import { takePreset } from "../ipc";
import { normalizeAddress } from "../logic/format";
import { connectErrorText, errorText } from "../logic/setup";
import type { ConnectError, ConnectFound } from "../types";
import { banner, button, caption, field, orgMark } from "../ui";

interface ConnectPage {
  address: string;
  phase: "idle" | "checking" | "found" | "error";
  found: ConnectFound | null;
  error: ConnectError | null;
}

let c: ConnectPage = { address: "", phase: "idle", found: null, error: null };
let presetRead = false;

export function render(ctx: Ctx): Node[] {
  if (!presetRead) {
    presetRead = true;
    const p = takePreset("connect");
    if (p) c = { ...c, ...p, found: p.found ?? null, error: p.error ?? null };
  }
  const conn = ctx.snap.connect;
  const back = button("Back", { variant: "ghost", id: "back", onclick: () => void goBack(ctx) });
  const body: Child[] = [
    h("div", { class: "heading" }, h("h1", { class: "h-setup" }, "Connect to a team"), h("p", { class: "lede" }, "Enter the address of a Kivali team running on another computer or a server.")),
  ];
  let buttons: Child[];
  if (c.phase === "found" && c.found) {
    const f = c.found;
    body.push(
      h(
        "div",
        { class: "status-card is-selected" },
        orgMark(f.name, 40),
        h("div", { class: "status-card-main" }, h("span", { class: "strong" }, `Found ${f.name}`), caption(f.host)),
      ),
    );
    if (conn.state === "not_invited") {
      const who = conn.email ? conn.email : "This Google account";
      body.push(
        banner("danger", `${who} can’t sign in to ${f.name}`, `Only ${f.name}’s owner can sign in. Use the owner’s Google account.`),
        button("Use a different account", { variant: "secondary", id: "other-account", onclick: () => void ctx.act("connect_reset") }),
      );
    } else {
      if (conn.state === "failed") body.push(banner("danger", "Couldn’t finish signing in", conn.error ?? "The browser didn’t come back to Kivali. Try again, and keep this window open."));
      body.push(
        h(
          "div",
          { class: "action-block" },
          button("Continue with Google", {
            variant: "primary",
            id: "google",
            loading: conn.state === "signing_in",
            onclick: () => void ctx.act("connect_signin", { origin: f.origin, name: f.name }),
          }),
          caption(`Use ${f.name}’s owner’s Google account. Opens your browser.`),
        ),
      );
    }
    buttons = [back];
  } else {
    const checking = c.phase === "checking";
    const f = field({
      id: "address",
      label: "Address",
      value: c.address,
      type: "url",
      inputmode: "url",
      placeholder: "https://",
      hint: checking ? null : "On that computer: Settings → the team → Other devices.",
      error: c.phase === "error" && c.error ? connectErrorText(c.error) : null,
      oninput: (v) => {
        c.address = v;
        const b = document.getElementById("connect") as HTMLButtonElement | null;
        if (b) b.disabled = !v.trim();
      },
      onenter: () => void check(ctx),
      onblur: (input) => {
        if (input.value.trim()) {
          c.address = normalizeAddress(input.value);
          input.value = c.address;
        }
      },
    });
    body.push(f.wrap);
    buttons = [
      back,
      button(checking ? "Checking" : "Connect", { variant: "primary", id: "connect", loading: checking, disabled: !c.address.trim(), onclick: () => void check(ctx) }),
    ];
  }
  if (ctx.notice) body.push(banner("danger", "Something went wrong", ctx.notice));
  return [
    h(
      "div",
      { class: "setup" },
      h("div", { class: "setup-body" }, ...body),
      h("div", { class: "setup-foot" }, h("span", { class: "mono-label" }), h("div", { class: "foot-actions" }, ...buttons)),
    ),
  ];
}

async function check(ctx: Ctx) {
  if (!c.address.trim() || c.phase === "checking") return;
  c.address = normalizeAddress(c.address);
  c.phase = "checking";
  c.error = null;
  ctx.rerender();
  try {
    const found = await ctx.act<ConnectFound>("connect_check", { address: c.address }, true);
    c.found = found ?? null;
    c.phase = found ? "found" : "idle";
  } catch (e) {
    c.phase = "error";
    c.error =
      e && typeof e === "object" && "kind" in e ? (e as ConnectError) : { kind: "invalid", message: errorText(e) };
  }
  ctx.rerender();
}

async function goBack(ctx: Ctx) {
  if (c.phase === "found") {
    c.phase = "idle";
    c.found = null;
    await ctx.act("connect_reset");
    ctx.rerender();
    return;
  }
  c = { address: "", phase: "idle", found: null, error: null };
  ctx.go(ctx.snap.teams.length ? "#/setup/add" : "#/setup/welcome");
}
