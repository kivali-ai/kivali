// Settings → General's Kivali row: the app's own update, in words.
// Pure (appupdate.test.ts).

import type { AppUpdate } from "../types";
import { ago } from "./format";

export interface AppUpdateLine {
  sub: string;
  /** The row's button: check now, a check under way, or update (E4). */
  action: "check" | "checking" | "update" | "retry" | null;
  highlight: boolean;
}

export function appUpdateLine(u: AppUpdate, now: Date): AppUpdateLine {
  const checked = u.checked_at ? ago(u.checked_at, now) : null;
  switch (u.state) {
    case "checking":
      return { sub: "Checking for updates…", action: "checking", highlight: false };
    case "up_to_date":
      return { sub: checked ? `Up to date · checked ${checked}` : "Up to date", action: "check", highlight: false };
    case "available":
      return { sub: `${u.version} is available`, action: "update", highlight: true };
    case "installing":
      return { sub: `Installing ${u.version ?? "the update"}…`, action: null, highlight: false };
    case "failed":
      return { sub: u.error ? `Couldn’t check for updates · ${u.error}` : "Couldn’t check for updates", action: "check", highlight: false };
    case "update_failed": {
      const what = u.version ? `Couldn’t update to ${u.version}` : "Couldn’t update";
      return { sub: u.error ? `${what} · ${u.error}` : what, action: "retry", highlight: true };
    }
    default:
      return { sub: "Not checked yet", action: "check", highlight: false };
  }
}
