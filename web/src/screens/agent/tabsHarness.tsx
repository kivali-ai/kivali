// Test harness for the agent's Background, About and Past chats tabs, the task page and Team: the real route
// table with fetch (answers may be promises the test settles), the org stream and any task stream faked.
// Used only by the tests next to it.
import { act, render } from '@testing-library/react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { vi } from 'vitest';
import type { ConnectionState, EventStream, StreamHandler } from '../../api/sse';
import type { AgentDetail, OrgSnapshot } from '../../api/types.gen';
import { FakeStream } from '../../app/test-utils';
import { createAppRoutes } from '../../app/router';
import { TooltipProvider } from '../../ds';
import { goMe, goSnapshot } from '../../state/fixtures';
import { goAgentDetail } from '../../state/fixtures/transcript';
import { ChatDepsContext } from './chatDeps';
import type { ChatDeps } from './chatDeps';
import { NOW } from './tabsFixtures';

export const SLUG = 'engineering-lead';

export interface Call {
  url: string;
  method: string;
  json: unknown;
}

/** A plain value is sent as 200 JSON; a Response as is; a function is called and may return either, or a promise of either. */
export type Route = unknown | ((call: Call) => unknown);

export function reply(status: number, body?: unknown): Response {
  return new Response(JSON.stringify(body ?? {}), { status, headers: { 'Content-Type': 'application/json' } });
}

export interface Deferred<T> {
  promise: Promise<T>;
  resolve(value: T): void;
}

export function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

/**
 * A task stream the test drives: FakeStream's surface plus an SSE id on each event, as the server puts the
 * count of file rows sent so far on every chat_message.
 */
export class TaskStream implements EventStream {
  state: ConnectionState = 'idle';
  started = false;
  stopped = false;
  private readonly handlers = new Map<string, Set<StreamHandler>>();

  on(event: string, handler: StreamHandler) {
    let set = this.handlers.get(event);
    if (!set) this.handlers.set(event, (set = new Set()));
    set.add(handler);
    return () => void set.delete(handler);
  }
  onState(handler: (s: ConnectionState) => void) {
    handler(this.state);
    return () => {};
  }
  start() {
    this.started = true;
    this.state = 'open';
  }
  stop() {
    this.stopped = true;
    this.state = 'idle';
  }
  emit(event: string, payload: unknown, id?: number) {
    const data = typeof payload === 'string' ? payload : JSON.stringify(payload);
    const ev = new MessageEvent(event, { data, ...(id === undefined ? {} : { lastEventId: String(id) }) });
    for (const h of this.handlers.get(event) ?? []) h(data, ev);
  }
}

export interface Harness {
  calls: Call[];
  org: FakeStream;
  /** Streams the page opened through the chat deps, oldest first. */
  streams: TaskStream[];
  streamUrls: string[];
  routes: Record<string, Route>;
  snapshot(s: OrgSnapshot): void;
  count(method: string, url: string): number;
}

export interface HarnessOptions {
  snapshot?: OrgSnapshot;
  detail?: AgentDetail;
  routes?: Record<string, Route>;
  /** The clock the pages read; fixed at NOW when the test gives none. */
  now?: () => number;
}

/** The snapshot with one agent's state and context replaced. */
export function withAgent(base: OrgSnapshot, slug: string, patch: Partial<OrgSnapshot['agents'][number]>): OrgSnapshot {
  return { ...base, agents: base.agents.map((a) => (a.slug === slug ? { ...a, ...patch } : a)) };
}

export function renderTabs(path: string, opts: HarnessOptions = {}): Harness {
  const calls: Call[] = [];
  const routes: Record<string, Route> = {
    'GET /api/v1/me': goMe,
    'GET /api/v1/snapshot': opts.snapshot ?? goSnapshot,
    ['GET /api/v1/agents/' + SLUG]: opts.detail ?? goAgentDetail,
    ...opts.routes,
  };
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      const method = (init?.method ?? 'GET').toUpperCase();
      const body = init?.body;
      const call: Call = { url, method, json: typeof body === 'string' ? (JSON.parse(body) as unknown) : null };
      calls.push(call);
      const hit = routes[method + ' ' + url];
      if (hit === undefined) return reply(404, { error: 'not found', who: 'Nobody' });
      const value = typeof hit === 'function' ? await (hit as (c: Call) => unknown)(call) : hit;
      return value instanceof Response ? value : reply(200, value);
    }),
  );

  const org = new FakeStream();
  const streams: TaskStream[] = [];
  const streamUrls: string[] = [];
  const deps: ChatDeps = {
    openStream(url) {
      const s = new TaskStream();
      streams.push(s);
      streamUrls.push(url);
      return s;
    },
    now: opts.now ?? (() => NOW),
  };
  const router = createMemoryRouter(createAppRoutes({ createStream: () => org, onSignOut: () => {} }), {
    basename: '/',
    initialEntries: [path],
  });
  render(
    <TooltipProvider>
      <ChatDepsContext.Provider value={deps}>
        <RouterProvider router={router} />
      </ChatDepsContext.Provider>
    </TooltipProvider>,
  );
  return {
    calls,
    org,
    streams,
    streamUrls,
    routes,
    snapshot: (s) => act(() => org.emit('snapshot', s)),
    count: (method, url) => calls.filter((c) => c.method === method && c.url === url).length,
  };
}
