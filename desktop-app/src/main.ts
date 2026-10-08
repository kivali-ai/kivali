// Kivali Desktop's bundled pages: setup (A), connect (B), the page in a
// team's window when the team can't show itself and Settings
// (D, with the E dialogs). They are the only pages with IPC; a team's own
// web app has none (docs/developers/desktop-app.md). Routes: logic/route.ts.

import "../../design-system/styles.css";
import "./style.css";

import type { Ctx } from "./ctx";
import { invoke, ipcReady, onShellChanged } from "./ipc";
import { parseRoute, setupTitle, type Route } from "./logic/route";
import { errorText } from "./logic/setup";
import { shouldPoll } from "./logic/teams";
import * as connect from "./pages/connect";
import * as settings from "./pages/settings";
import * as setup from "./pages/setup";
import * as team from "./pages/team";
import { initTheme } from "./theme";
import type { Snapshot } from "./types";

const root = document.getElementById("app")!;
let snap: Snapshot | null = null;
let lastJson = "";
let notice: string | null = null;
let lastTitle: string | null = null;
let pollTimer: number | undefined;

function route(): Route {
  return parseRoute(location.hash);
}

const ctx: Ctx = {
  get snap() {
    return snap!;
  },
  get route() {
    return route();
  },
  get now() {
    return new Date();
  },
  get notice() {
    return notice;
  },
  clearNotice() {
    notice = null;
  },
  fail(e: unknown) {
    const msg = errorText(e);
    if (msg === "cancelled") return;
    notice = msg;
    render();
  },
  rerender: () => render(),
  refresh: () => refresh(true),
  go(hash: string) {
    notice = null;
    if (location.hash === hash) render();
    else location.hash = hash;
  },
  async act<T>(cmd: string, args?: Record<string, unknown>, raw = false): Promise<T | undefined> {
    notice = null;
    try {
      const out = await invoke<T>(cmd, args);
      // The page acts on what the command did (a team it created, say)
      // as soon as this returns: the snapshot must already show it.
      await refresh(false).catch(() => {});
      return out;
    } catch (e) {
      await refresh(false).catch(() => {});
      if (raw) throw e;
      if (errorText(e) !== "cancelled") {
        notice = errorText(e);
        render();
      }
      return undefined;
    }
  },
};

async function refresh(force: boolean) {
  const next = await invoke<Snapshot>("shell_snapshot");
  const json = JSON.stringify(next);
  snap = next;
  schedulePoll();
  if (force || json !== lastJson) {
    lastJson = json;
    render();
  }
}

function schedulePoll() {
  window.clearTimeout(pollTimer);
  if (snap && shouldPoll(snap)) pollTimer = window.setTimeout(() => void refresh(false), 1000);
}

/** Redraws, keeping focus, the caret and scroll positions of elements with ids. */
function render() {
  if (!snap) return;
  const r = route();
  const active = document.activeElement as HTMLElement | null;
  const focusId = active && active !== document.body ? active.id : "";
  const caret =
    active instanceof HTMLInputElement && active.selectionStart !== null ? [active.selectionStart, active.selectionEnd ?? active.selectionStart] : null;
  const scrolls = new Map<string, number>();
  root.querySelectorAll<HTMLElement>("[data-keep-scroll]").forEach((el) => scrolls.set(el.id, el.scrollTop));

  const nodes =
    r.page === "setup" ? setup.render(ctx) : r.page === "connect" ? connect.render(ctx) : r.page === "team" ? team.render(ctx) : settings.render(ctx);
  root.replaceChildren(...nodes);

  for (const [id, top] of scrolls) {
    const el = document.getElementById(id);
    if (el) el.scrollTop = top;
  }
  if (focusId) {
    const el = document.getElementById(focusId);
    if (el && !el.hasAttribute("disabled")) {
      el.focus({ preventScroll: true });
      if (caret && el instanceof HTMLInputElement) {
        try {
          el.setSelectionRange(caret[0], caret[1]);
        } catch {
          // type=password/email may refuse; the caret goes to the end.
        }
      }
    }
  }
  const title = setupTitle(r);
  if (title && title !== lastTitle) {
    lastTitle = title;
    document.title = title;
    void invoke("set_title", { title }).catch(() => undefined);
  } else if (!title) {
    document.title = "Kivali";
  }
}

async function boot() {
  initTheme();
  await ipcReady();
  window.addEventListener("hashchange", () => {
    notice = null;
    render();
  });
  onShellChanged(() => void refresh(false));
  await refresh(true);
}

void boot().catch((e) => {
  root.textContent = errorText(e);
});

