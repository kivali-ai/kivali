// The page state a dev fixture starts from (mock.ts), so each state
// opens directly. Types only: nothing here ships behaviour.

import type { ConnectError, ConnectFound } from "./types";
import type { SetupState } from "./logic/setup";

export type DialogId = "pause" | "update" | "memory" | "remove" | "delete" | "delete-confirm";

export interface Preset {
  setup?: Partial<SetupState>;
  connect?: { address?: string; phase?: "idle" | "checking" | "found" | "error"; found?: ConnectFound; error?: ConnectError };
  dialog?: { id: DialogId; team: string; other?: string; typed?: string };
  /** Other devices with the address field open (after the switch). */
  devices?: { team: string; address: string; error?: string };
}
