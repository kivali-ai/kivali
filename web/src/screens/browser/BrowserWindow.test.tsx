import { act, render, screen } from '@testing-library/react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { BrowserWindowEvent, SubagentMeta, SubagentTranscript } from '../../api/types.gen';
import { createAppRoutes } from '../../app/router';
import { subagentTranscript } from '../agent/tabsFixtures';
import { browserSocketUrl } from './BrowserWindow';

const SLUG = 'engineering-lead';
const ID = 'bbbb2222';
const API = '/api/v1/agents/' + SLUG + '/subagents/' + ID;

/** A WebSocket the test drives: it records its URL and lets the test open it, push messages and drop it. */
class FakeSocket {
  static all: FakeSocket[] = [];
  binaryType = '';
  closed = false;
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  constructor(readonly url: string) {
    FakeSocket.all.push(this);
  }
  close() {
    if (this.closed) return;
    this.closed = true;
    this.onclose?.(new CloseEvent('close'));
  }
  accept() {
    act(() => this.onopen?.(new Event('open')));
  }
  send(ev: BrowserWindowEvent) {
    act(() => this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(ev) })));
  }
  frame(blob: Blob) {
    act(() => this.onmessage?.(new MessageEvent('message', { data: blob })));
  }
  /** The server went away without an end event. */
  drop() {
    act(() => this.close());
  }
}

interface FakeBitmap {
  width: number;
  height: number;
  name: string;
  close: ReturnType<typeof vi.fn>;
}

/** Decodes the test settles by hand, in any order. */
const decodes: { blob: Blob; resolve(b: FakeBitmap): void }[] = [];
const drawImage = vi.fn();
let reads = 0;
let meta: SubagentMeta;

const runningWeb: SubagentMeta = (() => {
  const { ended: _e, error: _r, ...m } = subagentTranscript.meta;
  return { ...m, state: 'running', kind: 'web', session: 'open', browser: 'starting' };
})();

function bitmap(name: string, width = 1280, height = 800): FakeBitmap {
  return { name, width, height, close: vi.fn() };
}

const latest = () => FakeSocket.all[FakeSocket.all.length - 1];

/** Lets pending promises (the header read, decodes) settle and React commit. */
async function settle(ms = 0) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

async function renderWindow(m: SubagentMeta = runningWeb) {
  meta = m;
  const router = createMemoryRouter(createAppRoutes(), { initialEntries: ['/agents/' + SLUG + '/subagents/' + ID + '/browser'] });
  render(<RouterProvider router={router} />);
  await settle();
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'requestAnimationFrame', 'cancelAnimationFrame'] });
  FakeSocket.all = [];
  decodes.length = 0;
  drawImage.mockClear();
  reads = 0;
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal(
    'createImageBitmap',
    vi.fn(
      (blob: Blob) =>
        new Promise<FakeBitmap>((resolve) => {
          decodes.push({ blob, resolve });
        }),
    ),
  );
  vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue({ drawImage } as unknown as CanvasRenderingContext2D);
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      if (url !== API) return { ok: false, status: 404, json: async () => ({ error: 'No such task.', who: 'Nobody' }) };
      reads++;
      const body: SubagentTranscript = { meta, rows: [] };
      return { ok: true, status: 200, json: async () => body };
    }),
  );
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('the browser window: connecting', () => {
  it('opens a same-origin socket to the task’s browser, frames as blobs', async () => {
    await renderWindow();
    expect(FakeSocket.all).toHaveLength(1);
    expect(latest()?.url).toBe('ws://' + window.location.host + API + '/browser');
    expect(latest()?.binaryType).toBe('blob');
    expect(screen.getByRole('status')).toHaveTextContent('Connecting to the browser');
    expect(document.title).toBe('Check the build logs · browser');
  });

  it('follows the page’s scheme', () => {
    expect(browserSocketUrl(SLUG, ID, { protocol: 'https:', host: 'kivali.test' } as Location)).toBe('wss://kivali.test' + API + '/browser');
    expect(browserSocketUrl(SLUG, ID, { protocol: 'http:', host: 'localhost:8080' } as Location)).toBe('ws://localhost:8080' + API + '/browser');
  });

  it('opens no socket for a general task', async () => {
    await renderWindow({ ...runningWeb, kind: 'general', session: undefined, browser: undefined });
    expect(screen.getByRole('status')).toHaveTextContent('This task does not use a browser.');
    expect(FakeSocket.all).toHaveLength(0);
  });

  it('opens no socket for a task that has ended', async () => {
    await renderWindow(subagentTranscript.meta);
    expect(screen.getByRole('status')).toHaveTextContent('The task has ended.');
    expect(FakeSocket.all).toHaveLength(0);
  });

  it('says so when the task cannot be read', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 404, json: async () => ({ error: 'No such task.', who: 'Whoever sent you the link.' }) })));
    await renderWindow();
    expect(screen.getByText('No such task.')).toBeInTheDocument();
    expect(FakeSocket.all).toHaveLength(0);
  });
});

describe('the browser window: the bar', () => {
  it('shows the tab’s host large, its URL and title, Watch only and the task', async () => {
    await renderWindow();
    latest()?.accept();
    latest()?.send({ type: 'tab', tab: { id: 't1', url: 'https://shop.example.com/cart?step=2', title: 'Your cart' } });
    expect(screen.getByRole('heading', { name: 'shop.example.com' })).toBeInTheDocument();
    expect(screen.getByText('https://shop.example.com/cart?step=2')).toBeInTheDocument();
    expect(screen.getByText('Your cart')).toBeInTheDocument();
    expect(screen.getByText('Watch only')).toBeInTheDocument();
    expect(screen.getByText('Check the build logs')).toBeInTheDocument();

    latest()?.send({ type: 'tab', tab: { id: 't2', url: 'https://docs.example.org/', title: 'Docs' } });
    expect(screen.getByRole('heading', { name: 'docs.example.org' })).toBeInTheDocument();
    expect(screen.queryByText('Your cart')).toBeNull();
  });
});

describe('the browser window: frames', () => {
  it('says the browser is starting until the first frame, then draws it once on the next animation frame', async () => {
    await renderWindow();
    latest()?.accept();
    expect(screen.getByRole('status')).toHaveTextContent('Starting the browser');
    latest()?.send({ type: 'state', browser: 'running' });
    expect(screen.getByRole('status')).toHaveTextContent('Starting the browser');

    const blob = new Blob(['jpeg'], { type: 'image/jpeg' });
    latest()?.frame(blob);
    expect(decodes).toHaveLength(1);
    expect(decodes[0]?.blob).toBe(blob);
    const one = bitmap('one');
    decodes[0]?.resolve(one);
    await settle();
    expect(drawImage).not.toHaveBeenCalled();

    await settle(16);
    expect(drawImage).toHaveBeenCalledTimes(1);
    expect(drawImage).toHaveBeenCalledWith(one, 0, 0);
    expect(one.close).toHaveBeenCalled();
    const canvas = screen.getByLabelText("The task's browser") as HTMLCanvasElement;
    expect([canvas.width, canvas.height]).toEqual([1280, 800]);
    expect(screen.queryByRole('status')).toBeNull();

    await settle(100);
    expect(drawImage).toHaveBeenCalledTimes(1);
  });

  it('draws only the newest of two frames decoded before a paint', async () => {
    await renderWindow({ ...runningWeb, browser: 'running' });
    latest()?.accept();
    latest()?.frame(new Blob(['a']));
    latest()?.frame(new Blob(['b']));
    const a = bitmap('a');
    const b = bitmap('b');
    decodes[0]?.resolve(a);
    decodes[1]?.resolve(b);
    await settle();
    expect(a.close).toHaveBeenCalled();
    await settle(16);
    expect(drawImage).toHaveBeenCalledTimes(1);
    expect(drawImage).toHaveBeenCalledWith(b, 0, 0);
  });

  it('drops a frame whose decode finishes after a newer one’s', async () => {
    await renderWindow({ ...runningWeb, browser: 'running' });
    latest()?.accept();
    latest()?.frame(new Blob(['old']));
    latest()?.frame(new Blob(['new']));
    const older = bitmap('old');
    const newer = bitmap('new');
    decodes[1]?.resolve(newer);
    await settle(16);
    decodes[0]?.resolve(older);
    await settle(16);
    expect(drawImage).toHaveBeenCalledTimes(1);
    expect(drawImage).toHaveBeenCalledWith(newer, 0, 0);
    expect(older.close).toHaveBeenCalled();
  });

  it('resizes the canvas only when the frame’s size changes', async () => {
    await renderWindow({ ...runningWeb, browser: 'running' });
    latest()?.accept();
    const canvas = screen.getByLabelText("The task's browser") as HTMLCanvasElement;
    latest()?.frame(new Blob(['a']));
    decodes[0]?.resolve(bitmap('a', 800, 600));
    await settle(16);
    expect([canvas.width, canvas.height]).toEqual([800, 600]);
    latest()?.frame(new Blob(['b']));
    decodes[1]?.resolve(bitmap('b', 1280, 720));
    await settle(16);
    expect([canvas.width, canvas.height]).toEqual([1280, 720]);
    expect(drawImage).toHaveBeenCalledTimes(2);
  });

  it('falls back to an image when createImageBitmap is missing', async () => {
    vi.stubGlobal('createImageBitmap', undefined);
    const created: string[] = [];
    const revoked: string[] = [];
    vi.spyOn(URL, 'createObjectURL').mockImplementation(() => {
      const u = 'blob:frame-' + created.length;
      created.push(u);
      return u;
    });
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation((u: string) => void revoked.push(u));
    await renderWindow({ ...runningWeb, browser: 'running' });
    latest()?.accept();
    latest()?.frame(new Blob(['a']));
    const img = screen.getByRole('img', { name: "The task's browser" }) as HTMLImageElement;
    expect(img.getAttribute('src')).toBe('blob:frame-0');
    expect(screen.queryByRole('status')).toBeNull();
    act(() => void img.dispatchEvent(new Event('load')));
    expect(revoked).toContain('blob:frame-0');
  });
});

describe('the browser window: ending and reconnecting', () => {
  it('shows the end’s reason and never reconnects', async () => {
    await renderWindow({ ...runningWeb, browser: 'running' });
    const sock = latest();
    sock?.accept();
    sock?.send({ type: 'end', reason: 'The task has ended.' });
    expect(screen.getByRole('status')).toHaveTextContent('The task has ended.');
    expect(sock?.closed).toBe(true);
    const before = reads;
    await settle(60_000);
    expect(FakeSocket.all).toHaveLength(1);
    expect(reads).toBe(before);
  });

  it('reconnects a dropped socket after 1 s, then 2 s, while the task runs', async () => {
    await renderWindow({ ...runningWeb, browser: 'running' });
    latest()?.accept();
    latest()?.drop();
    expect(screen.getByRole('status')).toHaveTextContent('Reconnecting');
    await settle(999);
    expect(FakeSocket.all).toHaveLength(1);
    await settle(1);
    expect(FakeSocket.all).toHaveLength(2);
    expect(screen.getByRole('status')).toHaveTextContent('Reconnecting');

    // Dropped again before it opened: the next wait is longer.
    latest()?.drop();
    await settle(1999);
    expect(FakeSocket.all).toHaveLength(2);
    await settle(1);
    expect(FakeSocket.all).toHaveLength(3);
    latest()?.accept();
    expect(screen.getByRole('status')).toHaveTextContent('Starting the browser');

    // An open resets the backoff.
    latest()?.drop();
    await settle(1000);
    expect(FakeSocket.all).toHaveLength(4);
  });

  it('stops reconnecting once the task has ended', async () => {
    await renderWindow({ ...runningWeb, browser: 'running' });
    latest()?.accept();
    meta = { ...subagentTranscript.meta };
    latest()?.drop();
    await settle(1000);
    expect(FakeSocket.all).toHaveLength(1);
    expect(screen.getByRole('status')).toHaveTextContent('The task has ended.');
    await settle(60_000);
    expect(FakeSocket.all).toHaveLength(1);
  });
});
