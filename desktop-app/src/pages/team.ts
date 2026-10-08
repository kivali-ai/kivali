// The Kivali page inside a team's window (#/team/<id>), shown only when the
// team can't show itself: paused, waking/pausing/updating, couldn't
// start, can't reach another computer. The shell
// swaps it for the team's web app once the team is running.

import type { Ctx } from "../ctx";
import { h, type Child } from "../h";
import { takePreset } from "../ipc";
import { memoryConflict, teamPage } from "../logic/teams";
import { banner, button, caption, orgMark, progress } from "../ui";
import { memorySheet } from "./dialogs";

let detailsOpen = false;
let conflict: string | null = null;
let presetRead = false;

export function render(ctx: Ctx): Node[] {
  const r = ctx.route;
  if (r.page !== "team") return [];
  if (!presetRead) {
    presetRead = true;
    const p = takePreset("dialog");
    if (p?.id === "memory") conflict = p.other ?? null;
  }
  const team = ctx.snap.teams.find((t) => t.id === r.id);
  const view = teamPage(team, ctx.snap, ctx.now);
  const name = team?.name ?? "";
  const body: Child[] = [orgMark(name || "?", 56, team?.place === "elsewhere")];
  const titles = (title: string, text: string | null) =>
    h("div", { class: "tp-titles" }, h("h1", { class: "h-page" }, title), text ? h("p", { class: "tp-body muted" }, text) : null);

  switch (view.kind) {
    case "paused":
      body.push(
        titles(view.title, view.body),
        button(view.action, { variant: "primary", id: "resume", onclick: () => void resume(ctx, r.id) }),
        view.since ? caption(view.since) : null,
      );
      break;
    case "progress":
      body.push(
        titles(view.title, view.body),
        h("div", { class: "tp-progress" }, progress(view.percent, "cobalt"), view.step ? caption(view.step) : null),
      );
      break;
    case "failed":
      body.push(
        titles(view.title, view.body),
        h(
          "div",
          { class: "action-row" },
          view.details
            ? button(detailsOpen ? "Hide details" : "Show details", {
                variant: "ghost",
                id: "details",
                onclick: () => {
                  detailsOpen = !detailsOpen;
                  ctx.rerender();
                },
              })
            : null,
          button(view.action.label, {
            variant: "primary",
            id: "retry",
            onclick: () =>
              void (view.action.pause
                ? ctx.act("pause_and_resume", { pause: view.action.pause, resume: r.id })
                : ctx.act("retry_team", { id: r.id })),
          }),
        ),
        detailsOpen && view.details ? h("pre", { class: "lines tp-lines", id: "tp-lines", "data-keep-scroll": true }, view.details) : null,
      );
      break;
    case "unreachable":
      body.push(
        titles(view.title, view.body),
        button("Try again", { variant: "secondary", id: "retry", onclick: () => void ctx.act("retry_team", { id: r.id }) }),
        view.last ? caption(view.last) : null,
      );
      break;
    case "gone":
      body.splice(0, 1);
      body.push(titles(view.title, view.body));
      break;
  }
  if (ctx.notice) body.push(banner("danger", "Something went wrong", ctx.notice));

  const out: Node[] = [h("div", { class: "team-page" }, h("div", { class: "tp-center" }, ...body))];
  if (conflict && team) {
    const other = ctx.snap.teams.find((t) => t.id === conflict);
    out.push(
      memorySheet(ctx, team, other, () => {
        conflict = null;
        ctx.rerender();
      }),
    );
  }
  return out;
}

async function resume(ctx: Ctx, id: string) {
  try {
    await ctx.act("resume_team", { id }, true);
  } catch (e) {
    const other = memoryConflict(e);
    if (other) {
      conflict = other;
      ctx.rerender();
    } else ctx.fail(e);
  }
}
