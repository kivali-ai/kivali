// A stand-in for the org provider that the tests of this folder and screens/graph drive by hand: mock
// '../../state/OrgProvider' with `useOrgStub`, then call `setOrg` to change what the screen reads.
import { act } from '@testing-library/react';
import { useSyncExternalStore } from 'react';
import { goSnapshot } from '../../state/fixtures';
import { initialOrgState, reduceSnapshot } from '../../state/org';
import type { OrgState } from '../../state/org';
import type { OrgContextValue } from '../../state/OrgProvider';

let value: Partial<OrgContextValue> = {};
const listeners = new Set<() => void>();

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => void listeners.delete(listener);
}

export function useOrgStub(): Partial<OrgContextValue> {
  return useSyncExternalStore(subscribe, () => value);
}

/** The org state after the golden snapshot: four agents, assignments_version 17. */
export function goOrg(over: Partial<OrgState> = {}): OrgState {
  return { ...reduceSnapshot(initialOrgState, goSnapshot), ...over };
}

export function setOrg(org: OrgState): void {
  act(() => {
    value = { org, me: null };
    for (const l of listeners) l();
  });
}

/** Makes matchMedia answer every min-width query with `on`: true is a desktop, false a phone. */
export function desktop(on: boolean): void {
  window.matchMedia = ((query: string) =>
    ({
      matches: on && query.includes('min-width'),
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }) as unknown as MediaQueryList) as typeof window.matchMedia;
}

/** Lets pending promise callbacks run inside act. */
export async function flush(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  });
}
