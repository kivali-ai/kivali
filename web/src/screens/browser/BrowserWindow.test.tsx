import { act, fireEvent, render, screen } from '@testing-library/react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { BrowserInput, BrowserWindowEvent, BrowserWindowInput, SubagentMeta, SubagentTranscript } from '../../api/types.gen';
import { createAppRoutes } from '../../app/router';
import { subagentTranscript } from '../agent/tabsFixtures';
import { MAX_FAILED_CONNECTS, browserSocketUrl, framePoint, modifierBits } from './BrowserWindow';

const SLUG = 'engineering-lead';
const ID = 'bbbb2222';
const API = '/api/v1/agents/' + SLUG + '/subagents/' + ID;
const UNREACHABLE = 'Kivali could not reach this task. Reopen it from the task page.';

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
  /** What the page sent, in order. */
  sent: string[] = [];
  send(data: string) {
    this.sent.push(data);
  }
  /** A text message from the server. */
  push(ev: BrowserWindowEvent) {
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

const SANDBOX = 'Chromium is running without its sandbox on this cluster.';

/** The fixture's task, running in the open session on a cluster that lets Chromium keep its sandbox. */
const runningWeb: SubagentMeta = (() => {
  const { ended: _e, error: _r, browser_sandbox_off: _s, ...m } = subagentTranscript.meta;
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
    latest()?.push({ type: 'tab', tab: { id: 't1', url: 'https://shop.example.com/cart?step=2', title: 'Your cart' } });
    expect(screen.getByRole('heading', { name: 'shop.example.com' })).toBeInTheDocument();
    expect(screen.getByText('https://shop.example.com/cart?step=2')).toBeInTheDocument();
    expect(screen.getByText('Your cart')).toBeInTheDocument();
    expect(screen.getByText('Watch only')).toBeInTheDocument();
    expect(screen.getByText('Check the build logs')).toBeInTheDocument();

    latest()?.push({ type: 'tab', tab: { id: 't2', url: 'https://docs.example.org/', title: 'Docs' } });
    expect(screen.getByRole('heading', { name: 'docs.example.org' })).toBeInTheDocument();
    expect(screen.queryByText('Your cart')).toBeNull();
    expect(screen.queryByText(SANDBOX)).toBeNull();
  });

  it('says when Chromium runs without its sandbox, from the header and from each state event', async () => {
    await renderWindow({ ...runningWeb, browser: 'running', browser_sandbox_off: true });
    expect(screen.getByText(SANDBOX)).toBeInTheDocument();
    latest()?.accept();
    latest()?.push({ type: 'state', browser: 'running', sandbox_off: true });
    expect(screen.getByText(SANDBOX)).toBeInTheDocument();
    // A browser that came back with its sandbox: the state event says nothing of it, and the line goes.
    latest()?.push({ type: 'state', browser: 'running' });
    expect(screen.queryByText(SANDBOX)).toBeNull();
    latest()?.push({ type: 'state', browser: 'running', sandbox_off: true });
    expect(screen.getByText(SANDBOX)).toBeInTheDocument();
  });
});

describe('the browser window: frames', () => {
  it('says the browser is starting until the first frame, then draws it once on the next animation frame', async () => {
    await renderWindow();
    latest()?.accept();
    expect(screen.getByRole('status')).toHaveTextContent('Starting the browser');
    latest()?.push({ type: 'state', browser: 'running' });
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

  it('shows the note a state event carries in place of its own caption, until the next state event', async () => {
    await renderWindow({ ...runningWeb, browser: 'running' });
    latest()?.accept();
    expect(screen.getByRole('status')).toHaveTextContent('Starting the browser');
    latest()?.push({ type: 'state', browser: 'running', reason: 'The browser has no page to show.' });
    expect(screen.getByRole('status')).toHaveTextContent('The browser has no page to show.');
    latest()?.push({ type: 'state', browser: 'running' });
    expect(screen.getByRole('status')).toHaveTextContent('Starting the browser');
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
    sock?.push({ type: 'end', reason: 'The task has ended.' });
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

    // Watch only: the wait grows to 10 s and stays there.
    latest()?.drop();
    await settle(2000);
    latest()?.drop();
    await settle(4000);
    latest()?.drop();
    await settle(9999);
    expect(FakeSocket.all).toHaveLength(6);
    await settle(1);
    expect(FakeSocket.all).toHaveLength(7);
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

  it('gives up after five connects that never opened and says to reopen the window', async () => {
    // Core holds no job for the task (it restarted) while the task's header still says it waits: every socket
    // is refused before it opens, and the header read keeps saying to connect.
    await renderWindow(waitingWeb);
    // The header says the window takes input, so no wait is longer than 4 s.
    for (const wait of [1000, 2000, 4000, 4000]) {
      latest()?.drop();
      expect(screen.getByRole('status')).toHaveTextContent('Reconnecting');
      await settle(wait);
    }
    expect(FakeSocket.all).toHaveLength(MAX_FAILED_CONNECTS);
    latest()?.drop();
    expect(screen.getByRole('status')).toHaveTextContent(UNREACHABLE);
    expect(screen.getByText('Watch only')).toBeInTheDocument();
    const before = reads;
    await settle(60_000);
    expect(FakeSocket.all).toHaveLength(MAX_FAILED_CONNECTS);
    expect(reads).toBe(before);
  });

  it('counts a header that cannot be read as a failed connect, and starts over once a socket opens', async () => {
    await renderWindow({ ...runningWeb, browser: 'running' });
    latest()?.accept();
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new Error('offline'))));
    // The open socket's drop is not a failed connect; the four unreadable headers after it are.
    latest()?.drop();
    for (const wait of [1000, 2000, 4000, 10_000]) await settle(wait);
    expect(FakeSocket.all).toHaveLength(1);
    expect(screen.getByRole('status')).toHaveTextContent('Reconnecting');
    await settle(10_000);
    expect(screen.getByRole('status')).toHaveTextContent(UNREACHABLE);
    await settle(60_000);
    expect(FakeSocket.all).toHaveLength(1);
  });

  it('forgets the failures once a socket opens', async () => {
    await renderWindow({ ...runningWeb, browser: 'running' });
    for (const wait of [1000, 2000, 4000]) {
      latest()?.drop();
      await settle(wait);
    }
    expect(FakeSocket.all).toHaveLength(4);
    latest()?.accept();
    // The open socket's drop waits 1 s and counts no failure; three refused connects follow it.
    for (const wait of [1000, 2000, 4000, 10_000]) {
      latest()?.drop();
      await settle(wait);
    }
    expect(FakeSocket.all).toHaveLength(8);
    expect(screen.getByRole('status')).toHaveTextContent('Reconnecting');
  });
});

// ---- Input: a task waiting for sign-in in a site session ----

const SITE = 'vendor.example';
const waitingWeb: SubagentMeta = { ...runningWeb, state: 'waiting_for_sign_in', session: SITE, site: SITE, browser: 'running', wait_reason: 'sign_in' };
const BANNER = 'Sign in, then close this window. The task continues on its own.';

/** The canvas's CSS box: 800 × 400 at (20, 40), so a 1280 × 800 frame shows at half size, 80 px in from each side. */
const BOX = { left: 20, top: 40, width: 800, height: 400, right: 820, bottom: 440, x: 20, y: 40, toJSON: () => ({}) } as DOMRect;

const sentInputs = (): BrowserInput[] =>
  (latest()?.sent ?? []).map((s) => {
    const msg = JSON.parse(s) as BrowserWindowInput;
    expect(msg.type).toBe('input');
    return msg.input;
  });

const frameEl = () => screen.getByLabelText("The task's browser") as HTMLCanvasElement;
/** The hidden editable the person's keys, compositions and pastes land in. */
const keysEl = () => screen.getByRole('textbox', { name: "Keys for the task's browser" });

/** Opens the window on a waiting task, paints one 1280 × 800 frame and, unless told not to, turns input on. */
async function openForInput(input = true, m: SubagentMeta = waitingWeb) {
  await renderWindow(m);
  latest()?.accept();
  latest()?.frame(new Blob(['jpeg']));
  decodes[0]?.resolve(bitmap('one'));
  await settle(16);
  latest()?.push({ type: 'state', browser: 'running', input, site: SITE });
  return frameEl();
}

/** A paste event carrying `text`, as Cmd+V or Ctrl+V fires it. */
function pasteEvent(text: string): Event {
  const ev = new Event('paste', { bubbles: true, cancelable: true });
  Object.defineProperty(ev, 'clipboardData', { value: { getData: (type: string) => (type === 'text/plain' ? text : '') } });
  return ev;
}

function stubClipboard(clipboard: Partial<Clipboard> | undefined) {
  Object.defineProperty(navigator, 'clipboard', { value: clipboard, configurable: true });
}

describe('the browser window: input', () => {
  const realClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
  beforeEach(() => {
    vi.spyOn(HTMLCanvasElement.prototype, 'getBoundingClientRect').mockReturnValue(BOX);
  });
  afterEach(() => {
    if (realClipboard) Object.defineProperty(navigator, 'clipboard', realClipboard);
    else delete (navigator as { clipboard?: Clipboard }).clipboard;
  });

  it('connects to a task waiting for sign-in', async () => {
    await renderWindow(waitingWeb);
    expect(FakeSocket.all).toHaveLength(1);
  });

  it('sends nothing, keeps the native menu and says Watch only until a state event turns input on', async () => {
    const canvas = await openForInput(false);
    const keys = keysEl();
    expect(screen.getByText('Watch only')).toBeInTheDocument();
    expect(screen.queryByText(BANNER)).toBeNull();
    expect(keys).toHaveAttribute('tabindex', '-1');
    expect(keys).toHaveAttribute('contenteditable', 'false');
    fireEvent.mouseDown(canvas, { button: 0, clientX: 420, clientY: 240 });
    expect(document.activeElement).not.toBe(keys);
    fireEvent.mouseUp(canvas, { button: 0, clientX: 420, clientY: 240 });
    fireEvent.mouseMove(canvas, { clientX: 430, clientY: 250 });
    fireEvent.wheel(canvas, { deltaY: 100, clientX: 420, clientY: 240 });
    fireEvent.keyDown(keys, { key: 'a', code: 'KeyA' });
    fireEvent.compositionEnd(keys, { data: 'é' });
    fireEvent.input(keys, { data: 'é', inputType: 'insertText' });
    expect(fireEvent.contextMenu(canvas, { clientX: 420, clientY: 240 })).toBe(true);
    const paste = pasteEvent('hunter2');
    act(() => void keys.dispatchEvent(paste));
    expect(paste.defaultPrevented).toBe(false);
    await settle(16);
    expect(latest()?.sent).toEqual([]);
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('says Taking input with the banner while input is on, and goes back when it turns off', async () => {
    const canvas = await openForInput();
    expect(screen.getByText('Taking input')).toBeInTheDocument();
    expect(screen.queryByText('Watch only')).toBeNull();
    expect(screen.getByText(BANNER)).toBeInTheDocument();
    expect(keysEl()).toHaveAttribute('tabindex', '0');
    expect(keysEl()).toHaveAttribute('contenteditable', 'true');

    latest()?.push({ type: 'state', browser: 'running', site: SITE });
    expect(screen.getByText('Watch only')).toBeInTheDocument();
    expect(screen.queryByText(BANNER)).toBeNull();
    fireEvent.mouseDown(canvas, { button: 0, clientX: 420, clientY: 240 });
    fireEvent.keyDown(keysEl(), { key: 'a', code: 'KeyA' });
    expect(latest()?.sent).toEqual([]);
  });

  it('stops taking input when the socket drops, until the new socket’s state says so', async () => {
    await openForInput();
    latest()?.drop();
    expect(screen.getByText('Watch only')).toBeInTheDocument();
    fireEvent.keyDown(keysEl(), { key: 'a', code: 'KeyA' });
    expect(FakeSocket.all[0]?.sent).toEqual([]);
  });

  it('waits at most 4 s between reconnects while the window takes input, so the task is not resumed meanwhile', async () => {
    await openForInput();
    expect(screen.getByText('Taking input')).toBeInTheDocument();
    for (const wait of [1000, 2000, 4000]) {
      latest()?.drop();
      await settle(wait);
    }
    expect(FakeSocket.all).toHaveLength(4);
    latest()?.drop();
    await settle(3999);
    expect(FakeSocket.all).toHaveLength(4);
    await settle(1);
    expect(FakeSocket.all).toHaveLength(5);

    // The task stops waiting for the person (its header and the state event agree): the waits may grow to 10 s again.
    meta = { ...waitingWeb, state: 'running', input: false, wait_reason: undefined };
    latest()?.accept();
    latest()?.push({ type: 'state', browser: 'running', site: SITE });
    for (const wait of [1000, 2000, 4000]) {
      latest()?.drop();
      await settle(wait);
    }
    latest()?.drop();
    await settle(9999);
    expect(FakeSocket.all).toHaveLength(8);
    await settle(1);
    expect(FakeSocket.all).toHaveLength(9);
  });

  it('sends presses and releases in frame pixels, focusing the keys under the pointer', async () => {
    const canvas = await openForInput();
    // (420, 240) is the middle of the box: the middle of the frame.
    fireEvent.mouseDown(canvas, { button: 0, clientX: 420, clientY: 240, shiftKey: true });
    const keys = keysEl();
    expect(document.activeElement).toBe(keys);
    expect([keys.style.left, keys.style.top]).toEqual(['400px', '200px']);
    fireEvent.mouseUp(window, { button: 0, clientX: 120, clientY: 60 });
    fireEvent.mouseDown(canvas, { button: 2, clientX: 420, clientY: 240 });
    fireEvent.mouseUp(canvas, { button: 2, clientX: 420, clientY: 240 });
    fireEvent.mouseDown(canvas, { button: 1, clientX: 420, clientY: 240 });
    expect(sentInputs()).toEqual([
      { kind: 'mouse', action: 'pressed', x: 640, y: 400, button: 'left', modifiers: 8 },
      // Released on the window, not the frame: the press still ends, and (120, 60) is (40, 40) in frame pixels.
      { kind: 'mouse', action: 'released', x: 40, y: 40, button: 'left', modifiers: 0 },
      { kind: 'mouse', action: 'pressed', x: 640, y: 400, button: 'right', modifiers: 0 },
      { kind: 'mouse', action: 'released', x: 640, y: 400, button: 'right', modifiers: 0 },
      { kind: 'mouse', action: 'pressed', x: 640, y: 400, button: 'middle', modifiers: 0 },
    ]);
  });

  it('coalesces moves to the newest one per animation frame, sent ahead of a press', async () => {
    const canvas = await openForInput();
    fireEvent.mouseMove(canvas, { clientX: 110, clientY: 50 });
    fireEvent.mouseMove(canvas, { clientX: 120, clientY: 60 });
    fireEvent.mouseMove(canvas, { clientX: 130, clientY: 70, buttons: 1 });
    expect(latest()?.sent).toEqual([]);
    await settle(16);
    expect(sentInputs()).toEqual([{ kind: 'mouse', action: 'moved', x: 60, y: 60, button: 'left', modifiers: 0 }]);

    fireEvent.mouseMove(canvas, { clientX: 140, clientY: 80 });
    fireEvent.mouseDown(canvas, { button: 0, clientX: 140, clientY: 80 });
    expect(sentInputs().slice(1)).toEqual([
      { kind: 'mouse', action: 'moved', x: 80, y: 80, modifiers: 0 },
      { kind: 'mouse', action: 'pressed', x: 80, y: 80, button: 'left', modifiers: 0 },
    ]);
    await settle(16);
    expect(sentInputs()).toHaveLength(3);
  });

  it('sends the wheel with its deltas and keeps the window from scrolling', async () => {
    const canvas = await openForInput();
    const wheel = new WheelEvent('wheel', { bubbles: true, cancelable: true, deltaX: 3, deltaY: 120, clientX: 420, clientY: 240 });
    act(() => void canvas.dispatchEvent(wheel));
    expect(wheel.defaultPrevented).toBe(true);
    expect(sentInputs()).toEqual([{ kind: 'mouse', action: 'wheel', x: 640, y: 400, delta_x: 3, delta_y: 120, modifiers: 0 }]);
  });

  it('sends a key’s key, code, typed text and modifiers, down and up, and keeps a typed key out of the editable', async () => {
    await openForInput();
    const keys = keysEl();
    expect(fireEvent.keyDown(keys, { key: 'A', code: 'KeyA', shiftKey: true })).toBe(false);
    fireEvent.keyUp(keys, { key: 'A', code: 'KeyA', shiftKey: true });
    expect(fireEvent.keyDown(keys, { key: 'a', code: 'KeyA', ctrlKey: true, altKey: true })).toBe(true);
    expect(sentInputs()).toEqual([
      { kind: 'key', action: 'down', key: 'A', code: 'KeyA', text: 'A', modifiers: 8 },
      { kind: 'key', action: 'up', key: 'A', code: 'KeyA', modifiers: 8 },
      // A shortcut types nothing.
      { kind: 'key', action: 'down', key: 'a', code: 'KeyA', modifiers: 3 },
    ]);
  });

  it('keeps the keys the window would act on for the page', async () => {
    await openForInput();
    const canvas = keysEl();
    for (const [key, code] of [
      ['Backspace', 'Backspace'],
      ['Tab', 'Tab'],
      ['Enter', 'Enter'],
      [' ', 'Space'],
      ['ArrowDown', 'ArrowDown'],
    ] as const) {
      expect(fireEvent.keyDown(canvas, { key, code })).toBe(false);
    }
    expect(sentInputs().map((i) => [i.key, i.text])).toEqual([
      ['Backspace', undefined],
      ['Tab', undefined],
      ['Enter', undefined],
      [' ', ' '],
      ['ArrowDown', undefined],
    ]);
  });

  it('lets Cmd+V and Ctrl+V through to the paste event, which sends the clipboard’s text', async () => {
    await openForInput();
    const keys = keysEl();
    expect(fireEvent.keyDown(keys, { key: 'v', code: 'KeyV', metaKey: true })).toBe(true);
    expect(fireEvent.keyDown(keys, { key: 'v', code: 'KeyV', ctrlKey: true })).toBe(true);
    expect(latest()?.sent).toEqual([]);
    const paste = pasteEvent('hunter2');
    act(() => void keys.dispatchEvent(paste));
    expect(paste.defaultPrevented).toBe(true);
    expect(sentInputs()).toEqual([{ kind: 'paste', text: 'hunter2' }]);
  });

  it('sends a dead key’s composition as a paste once committed: Option+e, then e, types é', async () => {
    await openForInput();
    const keys = keysEl();
    // The dead key and the keys under the composition are the input method's: not sent, not prevented.
    expect(fireEvent.keyDown(keys, { key: 'Dead', code: 'KeyE', altKey: true })).toBe(true);
    fireEvent.compositionStart(keys, { data: '' });
    fireEvent.compositionUpdate(keys, { data: '´' });
    fireEvent.input(keys, { data: '´', inputType: 'insertCompositionText', isComposing: true });
    fireEvent.keyUp(keys, { key: 'Dead', code: 'KeyE', altKey: true });
    expect(fireEvent.keyDown(keys, { key: 'e', code: 'KeyE', isComposing: true })).toBe(true);
    fireEvent.input(keys, { data: 'é', inputType: 'insertCompositionText', isComposing: true });
    expect(latest()?.sent).toEqual([]);
    fireEvent.compositionEnd(keys, { data: 'é' });
    expect(sentInputs()).toEqual([{ kind: 'paste', text: 'é' }]);
    // Safari reports the committed text once more, after the composition: not sent twice.
    fireEvent.input(keys, { data: 'é', inputType: 'insertFromComposition' });
    expect(sentInputs()).toHaveLength(1);
    expect(keys.textContent).toBe('');
  });

  it('lets an input method compose, candidates and all, and sends what it committed', async () => {
    await openForInput();
    const keys = keysEl();
    // The first key of a composition arrives before compositionstart, as Process.
    expect(fireEvent.keyDown(keys, { key: 'Process', code: 'KeyN' })).toBe(true);
    fireEvent.compositionStart(keys, { data: '' });
    fireEvent.keyDown(keys, { key: 'Process', code: 'KeyI', isComposing: true });
    // Space picks a candidate: the input method's, so the window neither sends nor prevents it.
    expect(fireEvent.keyDown(keys, { key: ' ', code: 'Space', isComposing: true })).toBe(true);
    fireEvent.compositionEnd(keys, { data: '日本' });
    expect(sentInputs()).toEqual([{ kind: 'paste', text: '日本' }]);
    // After the composition, keys are keys again.
    fireEvent.keyDown(keys, { key: 'Enter', code: 'Enter' });
    expect(sentInputs()[1]).toMatchObject({ kind: 'key', key: 'Enter' });
  });

  it('sends text that reaches the editable without a key of its own as a paste', async () => {
    await openForInput();
    const keys = keysEl();
    // The accent picker (press and hold) and an emoji picker insert text with no key behind it.
    fireEvent.input(keys, { data: 'é', inputType: 'insertText' });
    fireEvent.input(keys, { data: '→', inputType: 'insertText' });
    expect(sentInputs()).toEqual([
      { kind: 'paste', text: 'é' },
      { kind: 'paste', text: '→' },
    ]);
    expect(keys.textContent).toBe('');
  });

  it('offers Paste on a right-click, which reads the clipboard and sends it', async () => {
    const readText = vi.fn(async () => 'correct horse');
    stubClipboard({ readText });
    const canvas = await openForInput();
    expect(fireEvent.contextMenu(canvas, { clientX: 300, clientY: 200 })).toBe(false);
    const menu = screen.getByRole('menu');
    expect(menu.style.left).toBe('300px');
    expect(menu.style.top).toBe('200px');
    fireEvent.click(screen.getByRole('menuitem', { name: 'Paste' }));
    await settle();
    expect(readText).toHaveBeenCalledTimes(1);
    expect(sentInputs()).toEqual([{ kind: 'paste', text: 'correct horse' }]);
    expect(screen.queryByRole('menu')).toBeNull();
    expect(document.activeElement).toBe(keysEl());
  });

  it('says to use the shortcut when the clipboard cannot be read', async () => {
    stubClipboard(undefined);
    const canvas = await openForInput();
    fireEvent.contextMenu(canvas, { clientX: 300, clientY: 200 });
    expect(screen.getByRole('menuitem', { name: 'Use Cmd+V or Ctrl+V to paste' })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: 'Paste' })).toBeNull();
  });

  it('says to use the shortcut when reading the clipboard fails', async () => {
    stubClipboard({ readText: vi.fn(async () => Promise.reject(new Error('denied'))) });
    const canvas = await openForInput();
    fireEvent.contextMenu(canvas, { clientX: 300, clientY: 200 });
    fireEvent.click(screen.getByRole('menuitem', { name: 'Paste' }));
    await settle();
    expect(screen.getByRole('menuitem', { name: 'Use Cmd+V or Ctrl+V to paste' })).toBeInTheDocument();
    expect(latest()?.sent).toEqual([]);
  });

  it('closes the menu on a press elsewhere, and when input stops', async () => {
    stubClipboard({ readText: vi.fn(async () => '') });
    const canvas = await openForInput();
    fireEvent.contextMenu(canvas, { clientX: 300, clientY: 200 });
    fireEvent.mouseDown(screen.getByText('Taking input'));
    expect(screen.queryByRole('menu')).toBeNull();

    fireEvent.contextMenu(canvas, { clientX: 300, clientY: 200 });
    expect(screen.getByRole('menu')).toBeInTheDocument();
    latest()?.push({ type: 'state', browser: 'running', site: SITE });
    expect(screen.queryByRole('menu')).toBeNull();
  });
});

describe('the browser window: input helpers', () => {
  it('maps a point through the letterbox to frame pixels, clamped', () => {
    expect(framePoint(BOX, 1280, 800, 420, 240)).toEqual({ x: 640, y: 400 });
    expect(framePoint(BOX, 1280, 800, 100, 40)).toEqual({ x: 0, y: 0 });
    // In the left letterbox: clamped to the frame's left edge.
    expect(framePoint(BOX, 1280, 800, 50, 240)).toEqual({ x: 0, y: 400 });
    expect(framePoint(BOX, 1280, 800, 9999, 9999)).toEqual({ x: 1279, y: 799 });
    expect(framePoint(BOX, 0, 0, 420, 240)).toBeNull();
  });

  it('writes modifiers as the wire’s bits', () => {
    expect(modifierBits({ altKey: true, ctrlKey: true, metaKey: true, shiftKey: true })).toBe(15);
    expect(modifierBits({ altKey: false, ctrlKey: false, metaKey: true, shiftKey: false })).toBe(4);
  });
});

describe('the browser window: the session’s site', () => {
  it('flags a host outside the site, and not the site or a subdomain of it', async () => {
    await renderWindow(waitingWeb);
    latest()?.accept();
    latest()?.push({ type: 'state', browser: 'running', input: true, site: SITE });
    latest()?.push({ type: 'tab', tab: { id: 't1', url: 'https://login.vendor.example/', title: 'Sign in' } });
    expect(screen.queryByText(/Outside/)).toBeNull();
    expect(screen.getByRole('heading', { name: 'login.vendor.example' })).not.toHaveClass('kv-text--signal');

    latest()?.push({ type: 'tab', tab: { id: 't1', url: 'https://accounts.idp.example/auth', title: 'Sign in' } });
    expect(screen.getByText('Outside vendor.example')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'accounts.idp.example' })).toHaveClass('kv-text--signal');
  });

  it('flags nothing in the open session', async () => {
    await renderWindow({ ...runningWeb, browser: 'running' });
    latest()?.accept();
    latest()?.push({ type: 'state', browser: 'running' });
    latest()?.push({ type: 'tab', tab: { id: 't1', url: 'https://anywhere.test/', title: 'Anywhere' } });
    expect(screen.queryByText(/Outside/)).toBeNull();
  });
});
