import { describe, expect, it } from "vitest";
import { appUpdateLine } from "./appupdate";
import type { AppUpdate } from "../types";

const now = new Date(2026, 9, 20, 15, 0);

function u(o: Partial<AppUpdate>): AppUpdate {
  return { state: "not_checked", version: null, checked_at: null, error: null, downloaded: null, total: null, ...o };
}

const MB = 1024 * 1024;

describe("appUpdateLine", () => {
  // A download or install that broke off must not read "Couldn't check
  // for updates": the person would think the check failed and not learn
  // that the update is there to try again. Without this test the two
  // failures could merge back into one line.
  it("words a failed update apart from a failed check", () => {
    expect(appUpdateLine(u({ state: "failed", error: "offline" }), now)).toEqual({
      sub: "Couldn’t check for updates · offline",
      action: "check",
      highlight: false,
    });
    expect(appUpdateLine(u({ state: "update_failed", version: "1.2.0", error: "The operation was canceled by the user." }), now)).toEqual({
      sub: "Couldn’t update to 1.2.0 · The operation was canceled by the user.",
      action: "retry",
      highlight: true,
    });
    expect(appUpdateLine(u({ state: "update_failed", error: "No newer Kivali is available any more." }), now).sub).toBe(
      "Couldn’t update · No newer Kivali is available any more.",
    );
  });

  // The row's other states, so a change to one line does not move another.
  it("words the other states", () => {
    expect(appUpdateLine(u({}), now).sub).toBe("Not checked yet");
    expect(appUpdateLine(u({ state: "checking" }), now).action).toBe("checking");
    expect(appUpdateLine(u({ state: "up_to_date", checked_at: new Date(2026, 9, 20, 12, 0).toISOString() }), now).sub).toBe("Up to date · checked 3 hours ago");
    expect(appUpdateLine(u({ state: "available", version: "1.2.0" }), now)).toEqual({ sub: "1.2.0 is available", action: "update", highlight: true });
    expect(appUpdateLine(u({ state: "installing", version: "1.2.0" }), now)).toEqual({ sub: "Installing 1.2.0…", action: null, highlight: false });
  });

  // The Windows download is about 850 MB and the teams keep working while
  // it runs. Without a bar of bytes against the download's length the
  // person sees "Downloading" for minutes with no way to tell a slow
  // connection from a stuck one.
  it("shows the download's bytes against its length", () => {
    expect(appUpdateLine(u({ state: "downloading", version: "1.2.0", downloaded: 425 * MB, total: 850 * MB }), now)).toEqual({
      sub: "Downloading 1.2.0…",
      action: null,
      highlight: false,
      progress: { percent: 50, label: "425 of 850 MB" },
    });
    // No length from the server: the bytes so far, no bar.
    const unknown = appUpdateLine(u({ state: "downloading", version: "1.2.0", downloaded: 300 * MB }), now);
    expect(unknown.sub).toBe("Downloading 1.2.0… · 300 MB");
    expect(unknown.progress).toBeUndefined();
  });
});
