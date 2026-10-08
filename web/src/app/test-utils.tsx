import { render } from '@testing-library/react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { vi } from 'vitest';
import type { ConnectionState, EventStream, StreamHandler } from '../api/sse';
import type { Me, OrgSnapshot } from '../api/types.gen';
import { TooltipProvider } from '../ds';
import { goMe } from '../state/fixtures';
import { createAppRoutes } from './router';

/** An /org/stream stand-in the test drives by hand. */
export class FakeStream implements EventStream {
  state: ConnectionState = 'idle';
  started = false;
  stopped = false;
  private handlers = new Map<string, Set<StreamHandler>>();
  private stateHandlers = new Set<(s: ConnectionState) => void>();

  on(event: string, handler: StreamHandler) {
    let set = this.handlers.get(event);
    if (!set) this.handlers.set(event, (set = new Set()));
    set.add(handler);
    return () => void set.delete(handler);
  }
  onState(handler: (s: ConnectionState) => void) {
    this.stateHandlers.add(handler);
    handler(this.state);
    return () => void this.stateHandlers.delete(handler);
  }
  start() {
    this.started = true;
    this.setState('open');
  }
  stop() {
    this.stopped = true;
    this.setState('idle');
  }
  setState(s: ConnectionState) {
    this.state = s;
    for (const h of this.stateHandlers) h(s);
  }
  emit(event: string, payload: unknown) {
    const data = typeof payload === 'string' ? payload : JSON.stringify(payload);
    for (const h of this.handlers.get(event) ?? []) h(data, new MessageEvent(event, { data }));
  }
}

export interface FetchCall {
  url: string;
  method: string;
  body: string | null;
}

export type Responder = (call: FetchCall) => Response | unknown;

/** Stubs global fetch. Routes are keyed "METHOD /path"; a plain value is sent as 200 JSON. */
export function stubFetch(routes: Record<string, Responder | unknown>): FetchCall[] {
  const calls: FetchCall[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      const method = (init?.method ?? 'GET').toUpperCase();
      const body = typeof init?.body === 'string' ? init.body : null;
      const call = { url, method, body };
      calls.push(call);
      const hit = routes[method + ' ' + url];
      if (hit === undefined) return Promise.resolve(new Response(JSON.stringify({ error: 'not found', who: 'Nobody' }), { status: 404 }));
      const value = typeof hit === 'function' ? (hit as Responder)(call) : hit;
      if (value instanceof Response) return Promise.resolve(value);
      return Promise.resolve(new Response(JSON.stringify(value), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    }),
  );
  return calls;
}

export function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

export interface AppHarness {
  stream: FakeStream;
  signOut: ReturnType<typeof vi.fn>;
  calls: FetchCall[];
}

/** Renders the real route table at an app path (e.g. "/team") with fetch and the stream faked. */
export function renderApp(path: string, snapshot: OrgSnapshot, me: Me = goMe, extra: Record<string, Responder | unknown> = {}): AppHarness {
  const stream = new FakeStream();
  const signOut = vi.fn();
  const calls = stubFetch({
    'GET /api/v1/me': me,
    'GET /api/v1/snapshot': snapshot,
    'POST /api/v1/auto-release': (c: FetchCall) => JSON.parse(c.body ?? '{}'),
    ...extra,
  });
  const router = createMemoryRouter(createAppRoutes({ createStream: () => stream, onSignOut: signOut }), {
    basename: '/',
    initialEntries: [path],
  });
  render(
    <TooltipProvider>
      <RouterProvider router={router} />
    </TooltipProvider>,
  );
  return { stream, signOut, calls };
}
