// The in-page dialogs (pause, update, memory, remove, delete, and the API-key / Claude-account
// switches from the AI tab), drawn as sheets at the top of the window.

import type { Ctx } from "../ctx";
import { h } from "../h";
import { shortVersion } from "../logic/format";
import { deleteList, memoryDialog, osPromptLine, pauseText, thisComputer, updateText } from "../logic/teams";
import { deviceName } from "../logic/format";
import type { Team } from "../types";
import { button, caption, field, sheet } from "../ui";

const p = (text: string) => h("p", { class: "sheet-text" }, text);

/** E1. */
export function pauseSheet(ctx: Ctx, team: Team, close: () => void): HTMLElement {
  const ok = button("Pause", {
    variant: "primary",
    id: "sheet-ok",
    onclick: () => {
      close();
      void ctx.act("pause_team", { id: team.id });
    },
  });
  return sheet({
    key: `E1-${team.id}`,
    title: `Pause ${team.name}?`,
    body: [p(pauseText(team))],
    buttons: [button("Cancel", { variant: "ghost", id: "sheet-cancel", onclick: close }), ok],
    defaultButton: ok,
    onCancel: close,
  });
}

/** E3. */
export function updateSheet(ctx: Ctx, team: Team, close: () => void): HTMLElement {
  const to = shortVersion(team.update?.latest ?? null);
  const notes = team.update?.notes_url ?? null;
  const ok = button("Update", {
    variant: "primary",
    id: "sheet-ok",
    onclick: () => {
      close();
      void ctx.act("update_team", { id: team.id });
    },
  });
  return sheet({
    key: `E3-${team.id}`,
    title: to ? `Update ${team.name} to ${to}?` : `Update ${team.name}?`,
    body: [p(updateText(team))],
    buttons: [
      notes ? button("What’s new", { variant: "ghost", id: "sheet-notes", onclick: () => void ctx.act("open_external", { url: notes }) }) : null,
      button("Cancel", { variant: "ghost", id: "sheet-cancel", onclick: close }),
      ok,
    ].filter((b): b is HTMLButtonElement => !!b),
    defaultButton: ok,
    onCancel: close,
  });
}

/** Memory: resume wouldn't fit. */
export function memorySheet(ctx: Ctx, team: Team, other: Team | undefined, close: () => void): HTMLElement {
  const words = memoryDialog(team, other, ctx.snap.platform);
  const ok = button(words.confirm, {
    variant: "primary",
    id: "sheet-ok",
    onclick: () => {
      close();
      if (other) void ctx.act("pause_and_resume", { pause: other.id, resume: team.id });
    },
  });
  return sheet({
    key: `E5-${team.id}`,
    title: words.title,
    body: [p(words.body)],
    buttons: [button("Cancel", { variant: "ghost", id: "sheet-cancel", onclick: close }), ok],
    defaultButton: ok,
    onCancel: close,
  });
}

/** E6. */
export function removeSheet(ctx: Ctx, team: Team, close: () => void, done: () => void): HTMLElement {
  const ok = button("Remove", {
    variant: "primary",
    id: "sheet-ok",
    onclick: async () => {
      close();
      const r = await ctx.act("remove_team", { id: team.id });
      if (r !== undefined) done();
    },
  });
  return sheet({
    key: `E6-${team.id}`,
    title: `Remove ${team.name} from ${thisComputer(ctx.snap.platform)}?`,
    body: [p(`It keeps running on ${deviceName(team.device)}. You can connect again any time.`)],
    buttons: [button("Cancel", { variant: "ghost", id: "sheet-cancel", onclick: close }), ok],
    defaultButton: ok,
    onCancel: close,
  });
}

export type Facts = { agents: number | null; files: number | null; disk_used_bytes: number | null };

/** Delete: what goes. Keep is the default button. */
export function deleteSheet(ctx: Ctx, team: Team, facts: Facts | null, next: () => void, close: () => void): HTMLElement {
  const keep = button(`Keep ${team.name}`, { variant: "primary", id: "sheet-keep", onclick: close });
  return sheet({
    key: `E7-${team.id}`,
    title: `Delete ${team.name} from ${thisComputer(ctx.snap.platform)}?`,
    body: [p("This can’t be undone. Deleting removes:"), h("ul", { class: "sheet-list" }, ...deleteList(team, facts, ctx.snap.platform).map((t) => h("li", {}, t)))],
    buttons: [button("Continue…", { variant: "danger", id: "sheet-continue", onclick: next }), keep],
    defaultButton: keep,
    onCancel: close,
  });
}

/** Delete confirmation: say it. Delete forever stays off until the name matches exactly. */
export function confirmDeleteSheet(
  ctx: Ctx,
  team: Team,
  st: { typed: string; busy: boolean; error: string | null },
  onType: (v: string) => void,
  submit: () => void,
  close: () => void,
): HTMLElement {
  const del = button("Delete forever", { variant: "danger", id: "sheet-delete", disabled: st.typed !== team.name, loading: st.busy, onclick: submit });
  const f = field({
    id: "delete-name",
    label: "Team name",
    value: st.typed,
    error: st.error,
    oninput: (v) => {
      onType(v);
      del.disabled = v !== team.name;
    },
    onenter: () => {
      if (!del.disabled) submit();
    },
  });
  return sheet({
    key: `E8-${team.id}`,
    title: `Type ${team.name} to delete it`,
    body: [f.wrap, caption(osPromptLine(ctx.snap.platform))],
    buttons: [button("Cancel", { variant: "ghost", id: "sheet-cancel", onclick: close }), del],
    focus: f.input,
    onCancel: close,
  });
}

