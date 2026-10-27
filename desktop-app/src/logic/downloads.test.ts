import { describe, expect, it } from "vitest";
import { downloadLine, downloadPercent, ofWords, sizeWords } from "./downloads";
import type { Download } from "../types";

function dl(o: Partial<Download> = {}): Download {
  return { id: 1, name: "plan.pdf", folder: "Downloads", state: "downloading", received: 0, total: null, error: null, ...o };
}

describe("downloads", () => {
  // Sizes read in the unit a person would use; a chunked answer with no
  // length still says how much has arrived, so a slow download is not
  // mistaken for a stuck one.
  it("says sizes in sensible units", () => {
    expect(sizeWords(512)).toBe("512 B");
    expect(sizeWords(86016)).toBe("84 KB");
    expect(sizeWords(1258291)).toBe("1.2 MB");
    expect(sizeWords(3.4 * 1024 ** 3)).toBe("3.4 GB");
    expect(sizeWords(150 * 1024 ** 2)).toBe("150 MB");
    expect(ofWords(1258291, 4194304)).toBe("1.2 of 4.0 MB");
    expect(ofWords(0, 86016)).toBe("0 of 84 KB");
    expect(ofWords(2 * 1024 ** 3, 8 * 1024 ** 3)).toBe("2.0 of 8.0 GB");
  });

  // The bar is drawn only from a length the server gave; without one the
  // card shows an indeterminate bar, never a bar stuck at 0.
  it("has a percent only with a length", () => {
    expect(downloadPercent(dl({ received: 1024, total: 4096 }))).toBe(25);
    expect(downloadPercent(dl({ received: 5000, total: 4096 }))).toBe(100);
    expect(downloadPercent(dl({ received: 1024, total: null }))).toBeNull();
    expect(downloadPercent(dl({ received: 1024, total: 0 }))).toBeNull();
  });

  // The line under the name follows the state, naming the folder once
  // saved (so the person knows where to look) and the reason once failed.
  it("words each state", () => {
    expect(downloadLine(dl())).toBe("Starting");
    expect(downloadLine(dl({ received: 86016 }))).toBe("84 KB so far");
    expect(downloadLine(dl({ received: 1258291, total: 4194304 }))).toBe("1.2 of 4.0 MB");
    expect(downloadLine(dl({ state: "done", received: 4194304, total: 4194304 }))).toBe("Saved in Downloads");
    expect(downloadLine(dl({ state: "failed", error: "The connection was lost." }))).toBe("Couldn't download. The connection was lost.");
    expect(downloadLine(dl({ state: "failed" }))).toBe("Couldn't download.");
  });
});
