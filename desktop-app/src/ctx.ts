// What every page gets from main.ts: the snapshot, the route, and ways
// to act, re-fetch and redraw.

import type { Route } from "./logic/route";
import type { Snapshot } from "./types";

export interface Ctx {
  snap: Snapshot;
  route: Route;
  now: Date;
  /** Redraw from the current snapshot and page state. */
  rerender(): void;
  /** Re-fetch the snapshot, then redraw. */
  refresh(): Promise<void>;
  go(hash: string): void;
  /**
   * Runs a command. Errors are shown in the page's notice line and
   * swallowed (undefined), unless `raw` is set: then they are thrown.
   */
  act<T = unknown>(cmd: string, args?: Record<string, unknown>, raw?: boolean): Promise<T | undefined>;
  /** The last unexpected error, shown by the page until the next action. */
  notice: string | null;
  clearNotice(): void;
  /** Shows an error caught by the page in the notice line. */
  fail(e: unknown): void;
}
