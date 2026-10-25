import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { apiGet, apiPost } from '../../api/client';
import type { EventSourceLike } from '../../api/sse';
import type { SetupFlow } from '../../api/types.gen';
import { TooltipProvider } from '../../ds';
import { SetupFlowPanel } from './SetupFlowPanel';

vi.mock('../../api/client', async (importOriginal) => {
  const real = await importOriginal<typeof import('../../api/client')>();
  return { ...real, apiGet: vi.fn(), apiPost: vi.fn() };
});

const API = '/api/v1/org/integrations/github';
const ID = '9f86d081884c7d659a2feaa0c55ad015';

/** The flow's stream as the server would hold it open: states are sent by hand. */
class FakeSource implements EventSourceLike {
  readyState = 0;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  private listeners = new Map<string, Array<(ev: MessageEvent<string>) => void>>();
  constructor(readonly url: string) {}
  addEventListener(type: string, listener: (ev: MessageEvent<string>) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }
  close() {
    this.readyState = 2;
  }
  send(flow: SetupFlow) {
    for (const l of this.listeners.get('flow') ?? []) l(new MessageEvent('flow', { data: JSON.stringify(flow) }));
  }
}

function state(seq: number, rest: Omit<SetupFlow, 'id' | 'flow' | 'seq' | 'expires_at'>, id = ID): SetupFlow {
  return { id, flow: 'setup', seq, expires_at: '2026-10-21T10:10:00Z', ...rest };
}

const form = state(1, {
  state: 'form',
  step: { kind: 'form', words: 'Name the account.', fields: [{ key: 'account', title: 'Account', words: 'An organization, or blank', secret: false, setup_only: false }], no_return: false },
});
const running = state(2, { state: 'running' });
const go = state(3, {
  state: 'go',
  step: { kind: 'go', words: 'Create the app on GitHub.', fields: [], start_url: '/connect/start?flow=' + ID, host: 'github.com', no_return: false },
});

/** Lets every settled promise run and React commit what they set. */
async function flush() {
  await act(async () => {
    for (let i = 0; i < 20; i++) await Promise.resolve();
  });
}

const sources: FakeSource[] = [];

async function renderPanel() {
  render(
    <TooltipProvider>
      <SetupFlowPanel
        api={API}
        integration="GitHub"
        label="Install"
        onDone={async () => {}}
        openStream={(url) => {
          const s = new FakeSource(url);
          sources.push(s);
          return s;
        }}
      />
    </TooltipProvider>,
  );
  await flush();
  const stream = sources.at(-1);
  if (!stream) throw new Error('the panel opened no stream for the flow under way');
  return stream;
}

beforeEach(() => {
  sources.length = 0;
  vi.mocked(apiGet).mockImplementation(((path: string) => (path === API + '/setup' ? Promise.resolve(structuredClone(form)) : Promise.reject(new Error(path)))) as never);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('SetupFlowPanel', () => {
  // Guards the order of a request's reply and the stream. The server answers POST …/setup/answer with the flow as it
  // stands when the reply is written, running, while the step it set going may finish at once and reach the stream
  // first as the next step. Taken as it arrived, the stale reply would put the page back on "Working." with only
  // Cancel, and nothing publishes again while the flow waits on its Go step: the owner was stuck until the stream
  // reconnected. The page keeps the higher seq of the same flow, whichever came first.
  it('keeps the newer state the stream carried when an older reply lands after it', async () => {
    const stream = await renderPanel();
    expect(screen.getByLabelText('Account')).toBeInTheDocument();
    let reply: (f: SetupFlow) => void = () => {};
    vi.mocked(apiPost).mockImplementation((() => new Promise<SetupFlow>((resolve) => (reply = resolve))) as never);
    fireEvent.submit(screen.getByRole('button', { name: 'Continue' }).closest('form')!);
    await flush();
    act(() => stream.send(go));
    expect(screen.getByRole('button', { name: 'Continue to github.com' })).toBeInTheDocument();
    reply(running);
    await flush();
    expect(screen.getByRole('button', { name: 'Continue to github.com' })).toBeInTheDocument();
    expect(screen.queryByText('Working.')).toBeNull();
  });

  // Guards the usual order: the reply comes first and the stream moves the page on from it.
  it('moves on from a reply to the state the stream carries after it', async () => {
    const stream = await renderPanel();
    vi.mocked(apiPost).mockResolvedValue(structuredClone(running) as never);
    fireEvent.submit(screen.getByRole('button', { name: 'Continue' }).closest('form')!);
    await flush();
    expect(screen.getByText('Working.')).toBeInTheDocument();
    act(() => stream.send(go));
    expect(screen.getByRole('button', { name: 'Continue to github.com' })).toBeInTheDocument();
  });

  // Guards a flow started again: its numbering starts over at 1, under a new id, and the page must follow it rather
  // than hold the old flow's higher seq against it.
  it('takes a new flow whatever the old one’s seq', async () => {
    const stream = await renderPanel();
    act(() => stream.send(go));
    expect(screen.getByRole('button', { name: 'Continue to github.com' })).toBeInTheDocument();
    act(() => stream.send(state(1, { state: form.state, step: form.step }, 'a1b2c3d4e5f60718293a4b5c6d7e8f90')));
    expect(screen.getByLabelText('Account')).toBeInTheDocument();
  });
});
