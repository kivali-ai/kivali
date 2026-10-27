// The downloads card over a team's web app (#/downloads/<id>), in its
// own small webview at the window's top right (src-tauri/src/downloads.rs,
// windows.rs `sync_downloads`): one row per download, 64 px each inside
// 8 px of padding, which is the size the shell gives the webview. A
// download under way shows its bytes and Cancel; a saved one where it
// went and Show in folder; a failed one why, until dismissed.

import type { Ctx } from "../ctx";
import { cx, h } from "../h";
import { downloadLine, downloadPercent } from "../logic/downloads";
import type { Download } from "../types";
import { button } from "../ui";

export function render(ctx: Ctx): Node[] {
  const r = ctx.route;
  if (r.page !== "downloads") return [];
  const team = ctx.snap.teams.find((t) => t.id === r.id);
  const rows = (team?.downloads ?? []).map((d) => row(ctx, r.id, d));
  return [h("div", { class: "dl-card", role: "region", "aria-label": "Downloads" }, ...rows)];
}

function row(ctx: Ctx, team: string, d: Download): HTMLElement {
  const act = (cmd: string) => () => void ctx.act(cmd, { team, id: d.id });
  const dismiss = button("", { variant: "ghost", size: "sm", icon: "x", label: `Dismiss ${d.name}`, cls: "dl-x", onclick: act("dismiss_download") });
  const actions =
    d.state === "downloading"
      ? [button("Cancel", { variant: "ghost", size: "sm", onclick: act("cancel_download") })]
      : d.state === "done"
        ? [button("Show in folder", { variant: "ghost", size: "sm", onclick: act("reveal_download") }), dismiss]
        : [dismiss];
  return h(
    "div",
    { class: cx("dl-row", `dl-row--${d.state}`) },
    h(
      "div",
      { class: "dl-main" },
      h("div", { class: "dl-name", title: d.name }, d.name),
      bar(d),
      h("div", { class: cx("dl-line", "caption", d.state === "failed" ? "attention" : "muted") }, downloadLine(d)),
    ),
    h("div", { class: "dl-actions" }, ...actions),
  );
}

/** The bar of a download under way: its share when the length is known, else a moving band. */
function bar(d: Download): HTMLElement | null {
  if (d.state !== "downloading") return null;
  const pct = downloadPercent(d);
  const fill = h("span");
  if (pct !== null) fill.style.width = `${pct}%`;
  return h(
    "div",
    {
      class: cx("kv-progress", "kv-progress--cobalt", "dl-bar", pct === null && "dl-bar--indeterminate"),
      role: "progressbar",
      "aria-label": `Downloading ${d.name}`,
      "aria-valuemin": 0,
      "aria-valuemax": 100,
      "aria-valuenow": pct === null ? null : pct,
    },
    fill,
  );
}
