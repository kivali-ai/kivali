import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { ApiError, apiGet, apiPost } from '../api/client';
import { createEventStream } from '../api/sse';
import type { ConnectionState, EventStream } from '../api/sse';
import type { AutoRelease, Me, OrgSnapshot } from '../api/types.gen';
import { identifierFor } from '../lib/agentIdentity';
import type { Identify } from '../lib/agentIdentity';
import { initialOrgState, reduceSnapshot, withAutoRelease } from './org';
import type { OrgState } from './org';

export interface OrgContextValue {
  /** Who is signed in and which org; null until /api/v1/me answers. */
  me: Me | null;
  org: OrgState;
  connection: ConnectionState;
  /** Set when the first load failed for a reason other than being signed out. */
  loadError: ApiError | null;
  /** The last write that failed (auto-release today); the UI shows it and calls `dismissActionError`. */
  actionError: ApiError | null;
  dismissActionError(): void;
  setAutoRelease(value: AutoRelease): Promise<void>;
  /** Re-reads /api/v1/me, after the org's name or logo changes, so the sidebar and phone header follow. */
  refreshMe(): Promise<void>;
}

const OrgContext = createContext<OrgContextValue | null>(null);

export interface OrgProviderProps {
  children: ReactNode;
  /** Builds the /org/stream connection. Tests pass a fake. */
  createStream?: () => EventStream;
}

const defaultStream = () => createEventStream('/org/stream');

function asApiError(err: unknown): ApiError {
  return err instanceof ApiError ? err : new ApiError(0, 'Something went wrong in Kivali.', 'Reload the page. If it keeps happening, whoever runs this Kivali server can look into it.');
}

/**
 * Loads /api/v1/me and /api/v1/snapshot for first paint, then follows /org/stream. One provider serves the
 * whole frame; screens read it with `useOrg()`.
 */
export function OrgProvider({ children, createStream = defaultStream }: OrgProviderProps) {
  const [me, setMe] = useState<Me | null>(null);
  const [org, setOrg] = useState<OrgState>(initialOrgState);
  const [connection, setConnection] = useState<ConnectionState>('idle');
  const [loadError, setLoadError] = useState<ApiError | null>(null);
  const [actionError, setActionError] = useState<ApiError | null>(null);

  const orgRef = useRef(org);
  orgRef.current = org;
  const streamFactory = useRef(createStream);
  streamFactory.current = createStream;

  useEffect(() => {
    const abort = new AbortController();
    let stream: EventStream | null = null;
    let cancelled = false;

    Promise.all([
      apiGet<Me>('/api/v1/me', { signal: abort.signal }),
      apiGet<OrgSnapshot>('/api/v1/snapshot', { signal: abort.signal }),
    ])
      .then(([meBody, snapshot]) => {
        if (cancelled) return;
        setMe(meBody);
        setOrg((prev) => reduceSnapshot(prev, snapshot));
        stream = streamFactory.current();
        stream.on('snapshot', (data) => {
          let parsed: OrgSnapshot;
          try {
            parsed = JSON.parse(data) as OrgSnapshot;
          } catch {
            // A torn frame: the next snapshot replaces the whole state, so skipping one is safe.
            return;
          }
          setOrg((prev) => reduceSnapshot(prev, parsed));
        });
        stream.onState(setConnection);
        stream.start();
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        // Signed out: the client is already sending the browser to /login; a banner would only flash.
        if (err instanceof ApiError && err.status === 401) return;
        setLoadError(asApiError(err));
      });

    return () => {
      cancelled = true;
      abort.abort();
      stream?.stop();
    };
  }, []);

  const setAutoRelease = useCallback(async (value: AutoRelease) => {
    const previous = orgRef.current.autoRelease;
    if (previous === value) return;
    setOrg((prev) => withAutoRelease(prev, value));
    try {
      await apiPost('/api/v1/auto-release', { value });
    } catch (err) {
      setOrg((prev) => withAutoRelease(prev, previous));
      setActionError(asApiError(err));
    }
  }, []);

  const dismissActionError = useCallback(() => setActionError(null), []);

  // A failed re-read keeps the last good `me`: the change was saved, only the chrome is a step behind.
  const refreshMe = useCallback(async () => {
    try {
      setMe(await apiGet<Me>('/api/v1/me'));
    } catch {
      // keep what we had
    }
  }, []);

  const value = useMemo<OrgContextValue>(
    () => ({ me, org, connection, loadError, actionError, dismissActionError, setAutoRelease, refreshMe }),
    [me, org, connection, loadError, actionError, dismissActionError, setAutoRelease, refreshMe],
  );

  return <OrgContext.Provider value={value}>{children}</OrgContext.Provider>;
}

export function useOrg(): OrgContextValue {
  const ctx = useContext(OrgContext);
  if (!ctx) throw new Error('useOrg must be used inside <OrgProvider>');
  return ctx;
}

/**
 * How to draw an agent: its role icon and identity colour as the sidebar draws it, looked up by slug in the team
 * tree. Outside the provider (a test that mounts one screen) every agent keeps its slug's colour.
 */
export function useAgentIdentify(): Identify {
  const tree = useContext(OrgContext)?.org.tree;
  return useMemo(() => identifierFor(tree ?? []), [tree]);
}

/** Your name as the org chart shows it, on your messages and your circle: what your agents call you, else your account's name; "You" until /api/v1/me answers. */
export function usePersonName(): string {
  const me = useContext(OrgContext)?.me;
  return me?.org.owner_name || me?.user.name || 'You';
}
