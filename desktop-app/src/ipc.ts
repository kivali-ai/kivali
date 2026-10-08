// The pages' only way to the shell: `invoke` and the `shell-changed`
// event. Under `vite dev` in a plain browser (no Tauri), a mock answers
// from fixtures (mock.ts); the import is dead code in a build.

import { invoke as tauriInvoke } from "@tauri-apps/api/core";
import { listen as tauriListen } from "@tauri-apps/api/event";
import type { Preset } from "./preset";

type Invoke = <T>(cmd: string, args?: Record<string, unknown>) => Promise<T>;
type Listen = (event: string, cb: () => void) => Promise<unknown>;

let impl: { invoke: Invoke; listen: Listen } = {
  invoke: (cmd, args) => tauriInvoke(cmd, args),
  listen: (event, cb) => tauriListen(event, () => cb()),
};
let presetValue: Preset | null = null;

export async function ipcReady(): Promise<void> {
  if (import.meta.env.DEV && !("__TAURI_INTERNALS__" in window)) {
    const mock = await import("./mock");
    const m = mock.installMock();
    impl = { invoke: m.invoke, listen: m.listen };
    presetValue = m.preset;
  }
}

export function invoke<T = unknown>(cmd: string, args?: Record<string, unknown>): Promise<T> {
  return impl.invoke<T>(cmd, args);
}

export function onShellChanged(cb: () => void) {
  void impl.listen("shell-changed", cb);
}

/** The dev fixture's starting page state (null outside `vite dev`). Read once. */
export function takePreset<K extends keyof Preset>(key: K): Preset[K] | undefined {
  const v = presetValue?.[key];
  if (presetValue) delete presetValue[key];
  return v;
}
