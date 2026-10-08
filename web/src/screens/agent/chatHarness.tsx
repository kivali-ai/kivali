// Test harness for the agent pages: the real route table with fetch, the org stream and the agent stream
// faked. Used only by the tests next to it.
import { act, render } from '@testing-library/react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { vi } from 'vitest';
import type { AgentDetail, AgentState, Chat, OrgSnapshot } from '../../api/types.gen';
import { FakeStream } from '../../app/test-utils';
import { createAppRoutes } from '../../app/router';
import { TooltipProvider } from '../../ds';
import { goMe, goSnapshot } from '../../state/fixtures';
import { goAgentDetail } from '../../state/fixtures/transcript';
import type { Step } from '../../state/fixtures/transcript';
import { ChatDepsContext } from './chatDeps';
import type { ChatDeps } from './chatDeps';

export const SLUG = 'engineering-lead';
export const NOW = 1789923700000;

export interface Call {
  url: string;
  method: string;
  /** A JSON body parsed, a multipart body as FormData, else null. */
  json: unknown;
  form: FormData | null;
}

export type Route = unknown | ((call: Call) => unknown);

export function reply(status: number, body?: unknown): Response {
  if (status === 204) return new Response(null, { status });
  return new Response(JSON.stringify(body ?? {}), { status, headers: { 'Content-Type': 'application/json' } });
}

/**
 * The snapshot with this agent in `state` at `pct` context, with `waitingTasks` background tasks running
 * (one when the state is `waiting` and no count is given; a held agent is `waiting` with none).
 */
export function snapshotWith(state: AgentState, pct = 40, base: OrgSnapshot = goSnapshot, waitingTasks = state === 'waiting' ? 1 : 0): OrgSnapshot {
  return { ...base, agents: base.agents.map((a) => (a.slug === SLUG ? { ...a, state, context_pct: pct, waiting_tasks: waitingTasks } : a)) };
}

export interface Harness {
  calls: Call[];
  org: FakeStream;
  streams: FakeStream[];
  streamUrls: string[];
  /** The newest agent stream. */
  stream(): FakeStream;
  routes: Record<string, Route>;
  /** Emits a stream event inside act(). */
  emit(event: string, data: unknown, s?: FakeStream): void;
  /** Plays recorded steps into the newest agent stream. */
  play(steps: readonly Step[]): void;
  snapshot(s: OrgSnapshot): void;
}

export interface HarnessOptions {
  path?: string;
  chat?: Chat;
  detail?: AgentDetail;
  snapshot?: OrgSnapshot;
  routes?: Record<string, Route>;
}

export function renderAgent(opts: HarnessOptions = {}): Harness {
  const calls: Call[] = [];
  const routes: Record<string, Route> = {
    'GET /api/v1/me': goMe,
    'GET /api/v1/snapshot': opts.snapshot ?? snapshotWith('idle'),
    ['GET /api/v1/agents/' + SLUG]: opts.detail ?? goAgentDetail,
    ['GET /api/v1/agents/' + SLUG + '/chat']: opts.chat,
    ...opts.routes,
  };
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      const method = (init?.method ?? 'GET').toUpperCase();
      const body = init?.body;
      const call: Call = {
        url,
        method,
        json: typeof body === 'string' ? (JSON.parse(body) as unknown) : null,
        form: body instanceof FormData ? body : null,
      };
      calls.push(call);
      const hit = routes[method + ' ' + url];
      if (hit === undefined) return Promise.resolve(reply(404, { error: 'not found', who: 'Nobody' }));
      const value = typeof hit === 'function' ? (hit as (c: Call) => unknown)(call) : hit;
      // A route may answer with a promise, to hold a response until the test lets it go.
      return Promise.resolve(value).then((v) => (v instanceof Response ? v : reply(200, v)));
    }),
  );

  const org = new FakeStream();
  const streams: FakeStream[] = [];
  const streamUrls: string[] = [];
  const deps: ChatDeps = {
    openStream(url) {
      const s = new FakeStream();
      streams.push(s);
      streamUrls.push(url);
      return s;
    },
    now: () => NOW,
  };
  const router = createMemoryRouter(createAppRoutes({ createStream: () => org, onSignOut: () => {} }), {
    basename: '/',
    initialEntries: [(opts.path ?? '/agents/' + SLUG)],
  });
  render(
    <TooltipProvider>
      <ChatDepsContext.Provider value={deps}>
        <RouterProvider router={router} />
      </ChatDepsContext.Provider>
    </TooltipProvider>,
  );

  const newest = () => {
    const s = streams[streams.length - 1];
    if (!s) throw new Error('no agent stream was opened');
    return s;
  };
  const emit = (event: string, data: unknown, s?: FakeStream) => {
    act(() => (s ?? newest()).emit(event, data));
  };
  return {
    calls,
    org,
    streams,
    streamUrls,
    stream: newest,
    routes,
    emit,
    play(steps) {
      for (const step of steps) {
        if ('event' in step) emit(step.event, step.data);
        else if (step.action === 'stream_closed') act(() => newest().setState('closed'));
        else if (step.action === 'stream_open') act(() => newest().setState('open'));
      }
    },
    snapshot(s) {
      act(() => org.emit('snapshot', s));
    },
  };
}
