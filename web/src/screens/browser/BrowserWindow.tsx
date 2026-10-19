import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { useParams } from 'react-router';
import { apiGet, apiPost } from '../../api/client';
import type { BrowserInput, BrowserState, BrowserWindowEvent, BrowserWindowInput, BrowserWindowTab, SubagentMeta, SubagentTranscript, WebSignInReason } from '../../api/types.gen';
import { browserWindowDonePath, outsideSite } from '../../app/links';
import { Badge, Banner, Button, Icon, Text } from '../../ds';
import { asApiError } from '../agent/tabsShared';
import '../../styles/browser.css';

/** Waits before each reconnect, by attempt: 1 s, 2 s, 4 s, then every 10 s. */
export const RECONNECT_MS = [1000, 2000, 4000, 10_000];

/**
 * The longest wait while the window takes input. Core resumes a waiting task once its last window has been
 * gone for ten seconds (or at once when the person chooses Continue the task), so a window in the middle of a sign-in must be back well inside that.
 */
export const INPUT_RECONNECT_MAX_MS = 4000;

function reconnectDelay(attempt: number, input: boolean): number {
  const wait = RECONNECT_MS[Math.min(attempt, RECONNECT_MS.length - 1)] ?? 10_000;
  return input ? Math.min(wait, INPUT_RECONNECT_MAX_MS) : wait;
}

/**
 * Consecutive connects that never opened (the socket refused, or the task's header unreadable) before the
 * window stops trying: core no longer holds the task where the page was told it is.
 */
export const MAX_FAILED_CONNECTS = 5;

const UNREACHABLE = 'Kivali could not reach this task. Reopen it from the task page.';

/** The banner while the window takes input: what the page asks of the person, then the way on. */
export const FINISH: Record<WebSignInReason, string> = {
  sign_in: 'Sign in on this page, then continue the task.',
  mfa: 'Enter the second factor on this page, then continue the task.',
  captcha: 'Solve the CAPTCHA on this page, then continue the task.',
  consent: 'Answer the consent request on this page, then continue the task.',
};

/**
 * How the window leaves once the person has continued the task: it loads the done page in its place. The
 * desktop app closes its browser window on that load; a browser tab shows the done page, which closes the tab
 * where the browser lets a page close it. Tests replace `leave`.
 */
export const windowExit = {
  leave(path: string): void {
    window.location.replace(path);
  },
};

/**
 * The window once the person's part is over: the task continues without them. It tries to close itself, which
 * a browser allows for a window a page opened; where it stays open it says the task is continuing.
 */
export function BrowserWindowDone() {
  useEffect(() => {
    document.title = 'Browser';
    window.close();
  }, []);
  return <ContinuedNote />;
}

function ContinuedNote() {
  return (
    <div className="app-bw">
      <div className="app-bw-note">
        <Banner tone="success" title="The task is continuing.">
          You can close this window.
        </Banner>
      </div>
    </div>
  );
}

/** The same-origin WebSocket URL of a task's browser window, after the page's scheme. */
export function browserSocketUrl(slug: string, id: string, loc: Location = window.location): string {
  const scheme = loc.protocol === 'https:' ? 'wss:' : 'ws:';
  return scheme + '//' + loc.host + '/api/v1/agents/' + encodeURIComponent(slug) + '/subagents/' + encodeURIComponent(id) + '/browser';
}

/** True when core forwards the window's input: the task waits for a person in a site session, the same rule core applies to each message. */
function takesInput(meta: SubagentMeta): boolean {
  return meta.state === 'waiting_for_sign_in' && !!meta.site;
}

/** Why the window does not connect to this task, or null when it can. */
function unavailable(meta: SubagentMeta): string | null {
  if (meta.kind !== 'web') return 'This task does not use a browser.';
  if (meta.state === 'awaiting_approval') return 'The task has not started. It starts once you approve its uploads.';
  if (meta.state !== 'running' && meta.state !== 'waiting_for_sign_in') return 'The task has ended.';
  return null;
}

function hostOf(url: string): string {
  try {
    return new URL(url).host || url;
  } catch {
    return url;
  }
}

/** Whether a web page's URL lies outside the session's site. Only http and https pages are judged. */
function offSite(url: string, site: string): boolean {
  try {
    const u = new URL(url);
    return (u.protocol === 'http:' || u.protocol === 'https:') && outsideSite(u.hostname, site);
  } catch {
    return false;
  }
}

/** The wire's modifier bits: 1 alt, 2 control, 4 meta, 8 shift. */
export function modifierBits(ev: { altKey: boolean; ctrlKey: boolean; metaKey: boolean; shiftKey: boolean }): number {
  return (ev.altKey ? 1 : 0) | (ev.ctrlKey ? 2 : 0) | (ev.metaKey ? 4 : 0) | (ev.shiftKey ? 8 : 0);
}

/** MouseEvent.button to the wire's button: 0 left, 1 middle, 2 right. */
const BUTTON: Record<number, string> = { 0: 'left', 1: 'middle', 2: 'right' };

/** The button a move drags with, from MouseEvent.buttons (1 left, 2 right, 4 middle). */
function heldButton(buttons: number): string | undefined {
  if (buttons & 1) return 'left';
  if (buttons & 4) return 'middle';
  if (buttons & 2) return 'right';
  return undefined;
}

/** Keys the window itself would act on (scroll, move focus, press a button): they go to the task's page only. */
const PAGE_KEYS = new Set(['Backspace', 'Tab', 'Enter', ' ', 'ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight', 'PageUp', 'PageDown', 'Home', 'End']);

/** Keys that begin a composition (a dead key such as Option+e, an input method's Process): the composition's result goes to the page, not these. */
const COMPOSITION_KEYS = new Set(['Dead', 'Process']);

/** Cmd+V or Ctrl+V: left to the browser, which fires the paste event the window sends from. */
function pasteShortcut(ev: KeyboardEvent): boolean {
  return (ev.metaKey || ev.ctrlKey) && !ev.altKey && ev.key.toLowerCase() === 'v';
}

/**
 * A point in the frame's own pixels for a point on the page. The frame is drawn with object-fit: contain, so
 * the bitmap is scaled to fit the element's box and centred in it; the point is clamped to the bitmap.
 */
export function framePoint(box: DOMRect, width: number, height: number, clientX: number, clientY: number): { x: number; y: number } | null {
  if (width <= 0 || height <= 0) return null;
  const scale = Math.min(box.width / width, box.height / height);
  if (!(scale > 0)) return null;
  const left = box.left + (box.width - width * scale) / 2;
  const top = box.top + (box.height - height * scale) / 2;
  const clamp = (v: number, max: number) => Math.min(Math.max(v, 0), max);
  return { x: clamp((clientX - left) / scale, width - 1), y: clamp((clientY - top) / scale, height - 1) };
}

/** Wheel deltas in pixels, whatever unit the event counts in. */
function wheelPixels(ev: WheelEvent, height: number): { delta_x: number; delta_y: number } {
  const unit = ev.deltaMode === 1 ? 16 : ev.deltaMode === 2 ? height : 1;
  return { delta_x: ev.deltaX * unit, delta_y: ev.deltaY * unit };
}

/**
 * Forwards a person's mouse on the frame, and their keys, compositions and pastes in `keys`, to the task's
 * page, outside React: native listeners read whether the window takes input from `taking()` on every event
 * and call `send` directly. `keys` is a hidden editable: a press on the frame focuses it and moves it under
 * the pointer, so an input method's candidates open there. A key the person types goes to the page as itself
 * and is kept out of the editable; text the editable receives another way (an input method's or a dead key's
 * composition, an accent or emoji picker, dictation) goes as a paste once committed. Moves are coalesced to
 * the newest one per animation frame; a press, release or wheel sends any held move first so the page sees
 * them in order. Returns the function that removes the listeners.
 */
function relayInput(frame: HTMLElement, keys: HTMLElement, size: () => { width: number; height: number }, taking: () => boolean, send: (input: BrowserInput) => void, onMenu: (x: number, y: number) => void): () => void {
  let move: BrowserInput | null = null;
  let raf = 0;
  let composing = false;
  const pressed = new Set<string>();

  const point = (ev: MouseEvent) => {
    const { width, height } = size();
    return framePoint(frame.getBoundingClientRect(), width, height, ev.clientX, ev.clientY);
  };
  const flush = () => {
    if (raf) cancelAnimationFrame(raf);
    raf = 0;
    const m = move;
    move = null;
    if (m && taking()) send(m);
  };
  const button = (ev: MouseEvent, action: 'pressed' | 'released') => {
    const name = BUTTON[ev.button];
    const p = point(ev);
    if (!name || !p) return;
    flush();
    send({ kind: 'mouse', action, x: p.x, y: p.y, button: name, modifiers: modifierBits(ev) });
  };

  const onDown = (ev: MouseEvent) => {
    if (!taking()) return;
    ev.preventDefault();
    const box = frame.getBoundingClientRect();
    keys.style.left = ev.clientX - box.left + 'px';
    keys.style.top = ev.clientY - box.top + 'px';
    keys.focus({ preventScroll: true });
    const name = BUTTON[ev.button];
    if (name) pressed.add(name);
    button(ev, 'pressed');
  };
  // On the window, so a press dragged off the frame still ends.
  const onUp = (ev: MouseEvent) => {
    const name = BUTTON[ev.button];
    if (!name || !pressed.delete(name) || !taking()) return;
    button(ev, 'released');
  };
  const onMove = (ev: MouseEvent) => {
    if (!taking()) return;
    const p = point(ev);
    if (!p) return;
    const held = heldButton(ev.buttons);
    move = { kind: 'mouse', action: 'moved', x: p.x, y: p.y, modifiers: modifierBits(ev), ...(held ? { button: held } : {}) };
    if (!raf) raf = requestAnimationFrame(flush);
  };
  const onWheel = (ev: WheelEvent) => {
    if (!taking()) return;
    ev.preventDefault();
    const p = point(ev);
    if (!p) return;
    flush();
    send({ kind: 'mouse', action: 'wheel', x: p.x, y: p.y, ...wheelPixels(ev, size().height), modifiers: modifierBits(ev) });
  };
  const onContext = (ev: MouseEvent) => {
    if (!taking()) return;
    ev.preventDefault();
    onMenu(ev.clientX, ev.clientY);
  };
  const key = (ev: KeyboardEvent, action: 'down' | 'up') => {
    if (!taking() || pasteShortcut(ev)) return;
    // A composition's keys are the input method's; compositionend sends what it committed.
    if (ev.isComposing || composing || COMPOSITION_KEYS.has(ev.key)) return;
    const printable = [...ev.key].length === 1 && !ev.ctrlKey && !ev.metaKey;
    // The key goes to the page as itself: the window must neither act on it nor type it into the editable.
    if (PAGE_KEYS.has(ev.key) || printable) ev.preventDefault();
    send({ kind: 'key', action, key: ev.key, code: ev.code, ...(action === 'down' && printable ? { text: ev.key } : {}), modifiers: modifierBits(ev) });
  };
  const onKeyDown = (ev: KeyboardEvent) => key(ev, 'down');
  const onKeyUp = (ev: KeyboardEvent) => key(ev, 'up');
  const onCompositionStart = () => {
    composing = true;
  };
  const onCompositionEnd = (ev: CompositionEvent) => {
    composing = false;
    keys.textContent = '';
    if (ev.data && taking()) send({ kind: 'paste', text: ev.data });
  };
  // Text the editable received by other means than a key already sent (an accent picker, dictation, an emoji
  // picker). A composition's own input events are left to compositionend, whichever side of it they fall.
  const onInput = (ev: Event) => {
    const { isComposing, inputType, data } = ev as InputEvent;
    if (isComposing || composing || /Composition/.test(inputType ?? '')) return;
    keys.textContent = '';
    if (data && taking()) send({ kind: 'paste', text: data });
  };
  const onPaste = (ev: ClipboardEvent) => {
    if (!taking()) return;
    ev.preventDefault();
    const text = ev.clipboardData?.getData('text/plain') ?? '';
    if (text) send({ kind: 'paste', text });
  };

  frame.addEventListener('mousedown', onDown);
  window.addEventListener('mouseup', onUp);
  frame.addEventListener('mousemove', onMove);
  frame.addEventListener('wheel', onWheel, { passive: false });
  frame.addEventListener('contextmenu', onContext);
  keys.addEventListener('keydown', onKeyDown);
  keys.addEventListener('keyup', onKeyUp);
  keys.addEventListener('compositionstart', onCompositionStart);
  keys.addEventListener('compositionend', onCompositionEnd);
  keys.addEventListener('input', onInput);
  keys.addEventListener('paste', onPaste);
  return () => {
    if (raf) cancelAnimationFrame(raf);
    frame.removeEventListener('mousedown', onDown);
    window.removeEventListener('mouseup', onUp);
    frame.removeEventListener('mousemove', onMove);
    frame.removeEventListener('wheel', onWheel);
    frame.removeEventListener('contextmenu', onContext);
    keys.removeEventListener('keydown', onKeyDown);
    keys.removeEventListener('keyup', onKeyUp);
    keys.removeEventListener('compositionstart', onCompositionStart);
    keys.removeEventListener('compositionend', onCompositionEnd);
    keys.removeEventListener('input', onInput);
    keys.removeEventListener('paste', onPaste);
  };
}

/** Where the right-click menu opened, and whether the clipboard could not be read there. */
interface PasteMenu {
  x: number;
  y: number;
  fallback: boolean;
}

function canReadClipboard(): boolean {
  return typeof navigator.clipboard?.readText === 'function';
}

/**
 * Draws the newest frame and nothing else, outside React. Each frame takes a sequence number when it arrives;
 * its decode replaces the held bitmap only when no newer frame has finished decoding, and the canvas is painted
 * at most once per animation frame from whatever is held then. Replaced and painted bitmaps are closed. Where
 * createImageBitmap is missing, frames go to an image through an object URL instead, revoked once loaded.
 */
class FramePainter {
  private received = 0;
  private held = 0;
  private bitmap: ImageBitmap | null = null;
  private raf = 0;
  private done = false;
  private shown = false;

  constructor(
    private readonly canvas: HTMLCanvasElement | null,
    private readonly img: HTMLImageElement | null,
    private readonly onFirst: () => void,
  ) {
    if (img) img.onload = () => URL.revokeObjectURL(img.src);
  }

  push(blob: Blob): void {
    if (this.done) return;
    if (this.img) {
      if (this.img.src.startsWith('blob:')) URL.revokeObjectURL(this.img.src);
      this.img.src = URL.createObjectURL(blob);
      this.first();
      return;
    }
    const n = ++this.received;
    createImageBitmap(blob).then(
      (bmp) => {
        if (this.done || n <= this.held) {
          bmp.close();
          return;
        }
        this.bitmap?.close();
        this.bitmap = bmp;
        this.held = n;
        if (!this.raf) this.raf = requestAnimationFrame(this.paint);
      },
      () => {},
    );
  }

  private readonly paint = (): void => {
    this.raf = 0;
    const bmp = this.bitmap;
    const canvas = this.canvas;
    if (!bmp || !canvas) return;
    this.bitmap = null;
    if (canvas.width !== bmp.width) canvas.width = bmp.width;
    if (canvas.height !== bmp.height) canvas.height = bmp.height;
    canvas.getContext('2d', { alpha: false, desynchronized: true })?.drawImage(bmp, 0, 0);
    bmp.close();
    this.first();
  };

  private first(): void {
    if (this.shown) return;
    this.shown = true;
    this.onFirst();
  }

  stop(): void {
    this.done = true;
    if (this.raf) cancelAnimationFrame(this.raf);
    this.raf = 0;
    this.bitmap?.close();
    this.bitmap = null;
    if (this.img?.src.startsWith('blob:')) URL.revokeObjectURL(this.img.src);
  }
}

type Link = 'connecting' | 'open' | 'reconnecting' | 'ended';

/**
 * A web task's browser window, outside the app frame: the tab the task's browser shows, live. It reads the
 * task's header once; a live web task gets a WebSocket that carries the tab and the browser's state as text
 * and the frames as JPEGs. A socket that closes without an end event is reopened with backoff while the task
 * still runs. The window watches, and takes input only while the latest state event says so (the task waits
 * for a person to sign in): then the person's mouse, keys and pastes on the frame go back as input messages.
 */
export function BrowserWindow() {
  const { slug = '', id = '' } = useParams();
  const [meta, setMeta] = useState<SubagentMeta | null>(null);
  const [failure, setFailure] = useState<{ message: string; who: string } | null>(null);
  const [link, setLink] = useState<Link>('connecting');
  const [ended, setEnded] = useState('');
  const [browser, setBrowser] = useState<BrowserState | undefined>(undefined);
  const [sandboxOff, setSandboxOff] = useState(false);
  // What the latest state event said to show in place of the page's own caption ("The browser has no page to show.").
  const [note, setNote] = useState('');
  const [tab, setTab] = useState<BrowserWindowTab | null>(null);
  const [framed, setFramed] = useState(false);
  const [input, setInput] = useState(false);
  const [site, setSite] = useState('');
  const [menu, setMenu] = useState<PasteMenu | null>(null);
  const [reason, setReason] = useState<WebSignInReason | undefined>(undefined);
  // The person's Continue the task: sending, then done once core has resumed the task.
  const [continuing, setContinuing] = useState<'idle' | 'sending' | 'done'>('idle');
  const [continueError, setContinueError] = useState<{ message: string; who: string } | null>(null);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const imgRef = useRef<HTMLImageElement>(null);
  const keysRef = useRef<HTMLDivElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  // The input path reads these, never React state: whether the window takes input, and the send to use.
  const taking = useRef(false);
  const sendRef = useRef<(input: BrowserInput) => void>(() => {});
  // The last word on whether the window takes input, from the header or a state event: while the socket is
  // down it decides how long a reconnect may wait.
  const wantsInput = useRef(false);
  const [decodes] = useState(() => typeof createImageBitmap === 'function');

  const path = '/api/v1/agents/' + encodeURIComponent(slug) + '/subagents/' + encodeURIComponent(id);

  useEffect(() => {
    let live = true;
    apiGet<SubagentTranscript>(path).then(
      (t) => {
        if (!live) return;
        setMeta(t.meta);
        setBrowser(t.meta.browser);
        setSandboxOff(t.meta.browser_sandbox_off === true);
        setSite(t.meta.site ?? '');
        setReason(t.meta.wait_reason);
        wantsInput.current = takesInput(t.meta);
      },
      (err: unknown) => {
        if (live) setFailure(asApiError(err));
      },
    );
    return () => {
      live = false;
    };
  }, [path]);

  useEffect(() => {
    if (meta?.description) document.title = meta.description + ' · browser';
  }, [meta?.description]);

  // Input turning on is a wait beginning, maybe not the one the header was read in: the header says its reason.
  useEffect(() => {
    if (!input) return;
    let live = true;
    apiGet<SubagentTranscript>(path).then(
      (t) => {
        if (live && t.meta.wait_reason) setReason(t.meta.wait_reason);
      },
      () => {},
    );
    return () => {
      live = false;
    };
  }, [input, path]);

  const blocked = meta ? unavailable(meta) : null;
  const connect = meta !== null && blocked === null && continuing !== 'done';

  useEffect(() => {
    if (!connect) return;
    const canvas = canvasRef.current;
    const img = imgRef.current;
    let painted = false;
    const painter = new FramePainter(canvas, img, () => {
      painted = true;
      setFramed(true);
    });
    let ws: WebSocket | null = null;
    let live: WebSocket | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    // attempt counts the closes since the last open and sets the wait; failed counts the connects since then
    // that never opened, and ends the window at MAX_FAILED_CONNECTS.
    let attempt = 0;
    let failed = 0;
    let over = false;

    const take = (on: boolean) => {
      taking.current = on;
      setInput(on);
    };
    const send = (input: BrowserInput) => {
      if (!taking.current || !live) return;
      const msg: BrowserWindowInput = { type: 'input', input };
      live.send(JSON.stringify(msg));
    };
    sendRef.current = send;
    // The frame's own pixels: the canvas takes each painted bitmap's size; before the first paint there is none.
    const size = () => (canvas ? (painted ? { width: canvas.width, height: canvas.height } : { width: 0, height: 0 }) : { width: img?.naturalWidth ?? 0, height: img?.naturalHeight ?? 0 });
    const frame = canvas ?? img;
    const keys = keysRef.current;
    const unrelay = frame && keys ? relayInput(frame, keys, size, () => taking.current, send, (x, y) => setMenu({ x, y, fallback: !canReadClipboard() })) : () => {};

    const finish = (reason: string) => {
      over = true;
      take(false);
      setEnded(reason);
      setLink('ended');
    };

    const open = () => {
      const sock = new WebSocket(browserSocketUrl(slug, id));
      let opened = false;
      ws = sock;
      sock.binaryType = 'blob';
      sock.onopen = () => {
        opened = true;
        attempt = 0;
        failed = 0;
        live = sock;
        setLink('open');
      };
      sock.onmessage = (ev: MessageEvent) => {
        if (typeof ev.data !== 'string') {
          painter.push(ev.data as Blob);
          return;
        }
        let msg: BrowserWindowEvent;
        try {
          msg = JSON.parse(ev.data) as BrowserWindowEvent;
        } catch {
          return;
        }
        if (msg.type === 'state') {
          setBrowser(msg.browser);
          setSandboxOff(msg.sandbox_off === true);
          setNote(msg.reason ?? '');
          wantsInput.current = msg.input === true;
          take(msg.input === true);
          if (msg.site !== undefined) setSite(msg.site);
        } else if (msg.type === 'tab' && msg.tab) setTab(msg.tab);
        else if (msg.type === 'notice') setNote(msg.reason ?? '');
        else if (msg.type === 'end') {
          finish(msg.reason || 'The window has ended.');
          sock.close();
        }
      };
      sock.onclose = () => {
        if (live === sock) live = null;
        if (over || ws !== sock) return;
        ws = null;
        // A new socket's first state event says again whether the window takes input.
        take(false);
        if (!opened && ++failed >= MAX_FAILED_CONNECTS) {
          finish(UNREACHABLE);
          return;
        }
        setLink('reconnecting');
        timer = setTimeout(retry, reconnectDelay(attempt++, wantsInput.current));
      };
    };

    // The task may have ended while the socket was down; only a running web task is reconnected.
    const retry = () => {
      apiGet<SubagentTranscript>(path).then(
        (t) => {
          if (over) return;
          const why = unavailable(t.meta);
          wantsInput.current = takesInput(t.meta);
          if (why) finish(why);
          else open();
        },
        () => {
          if (over) return;
          if (++failed >= MAX_FAILED_CONNECTS) finish(UNREACHABLE);
          else timer = setTimeout(retry, reconnectDelay(attempt++, wantsInput.current));
        },
      );
    };

    open();
    return () => {
      over = true;
      taking.current = false;
      live = null;
      unrelay();
      clearTimeout(timer);
      if (ws) {
        ws.onclose = null;
        ws.close();
      }
      painter.stop();
    };
  }, [connect, slug, id, path]);

  // The menu belongs to input: it closes when input stops, on a press anywhere outside it, and on Escape.
  useEffect(() => {
    if (!input) setMenu(null);
  }, [input]);
  const menuOpen = menu !== null;
  // At the pointer: the menu is placed through its element, the app's styles keeping to classes.
  useLayoutEffect(() => {
    const el = menuRef.current;
    if (!el || !menu) return;
    el.style.left = menu.x + 'px';
    el.style.top = menu.y + 'px';
  }, [menu]);
  useEffect(() => {
    if (!menuOpen) return;
    const onDown = (ev: MouseEvent) => {
      if (!(ev.target instanceof Node && menuRef.current?.contains(ev.target))) setMenu(null);
    };
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key === 'Escape') setMenu(null);
    };
    document.addEventListener('mousedown', onDown, true);
    document.addEventListener('keydown', onKey, true);
    return () => {
      document.removeEventListener('mousedown', onDown, true);
      document.removeEventListener('keydown', onKey, true);
    };
  }, [menuOpen]);

  const pasteClipboard = () => {
    const read = canReadClipboard() ? navigator.clipboard.readText() : Promise.reject(new Error('no clipboard'));
    read.then(
      (text) => {
        if (text) sendRef.current({ kind: 'paste', text });
        setMenu(null);
        keysRef.current?.focus({ preventScroll: true });
      },
      () => setMenu((m) => m && { ...m, fallback: true }),
    );
  };

  const continueTask = () => {
    if (continuing !== 'idle') return;
    setContinuing('sending');
    setContinueError(null);
    apiPost<void>(path + '/continue').then(
      () => {
        setContinuing('done');
        windowExit.leave(browserWindowDonePath(slug, id));
      },
      (err: unknown) => {
        setContinuing('idle');
        setContinueError(asApiError(err));
      },
    );
  };

  if (continuing === 'done') return <ContinuedNote />;

  if (failure) {
    return (
      <div className="app-bw">
        <div className="app-bw-note">
          <Banner tone="danger" title={failure.message}>
            {failure.who}
          </Banner>
        </div>
      </div>
    );
  }

  let status = '';
  if (blocked) status = blocked;
  else if (!meta || link === 'connecting') status = 'Connecting to the browser';
  else if (link === 'reconnecting') status = 'Reconnecting';
  else if (link === 'ended') status = ended;
  else if (note) status = note;
  else if (!framed || browser === 'starting') status = 'Starting the browser';

  const off = tab !== null && offSite(tab.url, site);

  return (
    <div className="app-bw">
      <header className="app-bw-bar">
        <div className="app-bw-where">
          <Text as="h1" variant="heading" className="app-bw-host" {...(off ? { tone: 'signal' as const } : {})}>
            {tab ? hostOf(tab.url) : 'Browser'}
          </Text>
          {off && (
            <Text variant="label" tone="signal" className="app-bw-off">
              <Icon name="triangle-alert" size={14} />
              {'Outside ' + site}
            </Text>
          )}
          {tab && (
            <Text variant="caption" tone="muted" className="app-bw-url" title={tab.url}>
              {tab.url}
            </Text>
          )}
        </div>
        <div className="app-bw-about">
          {tab?.title && (
            <Text variant="body" className="app-bw-title">
              {tab.title}
            </Text>
          )}
          {input ? <Badge tone="signal">Taking input</Badge> : <Badge variant="outline">Watch only</Badge>}
          {meta?.description && (
            <Text variant="caption" tone="muted" className="app-bw-task">
              {meta.description}
            </Text>
          )}
        </div>
        {sandboxOff && (
          <Text variant="caption" tone="muted" className="app-bw-sandbox">
            Chromium is running without its sandbox on this cluster.
          </Text>
        )}
      </header>
      {input && (
        <div className="app-bw-banner">
          <Banner
            tone="info"
            title={FINISH[reason ?? 'sign_in']}
            action={
              <Button variant="primary" size="sm" onClick={continueTask} disabled={continuing === 'sending'} loading={continuing === 'sending'}>
                Continue the task
              </Button>
            }
          >
            Closing the window continues it too.
          </Banner>
        </div>
      )}
      {continueError && (
        <div className="app-bw-banner">
          <Banner tone="danger" title={continueError.message} onDismiss={() => setContinueError(null)}>
            {continueError.who}
          </Banner>
        </div>
      )}
      <main className="app-bw-view">
        {connect &&
          (decodes ? (
            <canvas ref={canvasRef} className="app-bw-frame" aria-label="The task's browser" />
          ) : (
            <img ref={imgRef} className="app-bw-frame" alt="The task's browser" draggable={false} />
          ))}
        {connect && (
          // Where keys are typed while the window takes input: hidden, focused by a press on the frame, editable
          // so an input method can compose in it (relayInput).
          <div
            ref={keysRef}
            className="app-bw-keys"
            role="textbox"
            aria-label="Keys for the task's browser"
            contentEditable={input}
            suppressContentEditableWarning
            tabIndex={input ? 0 : -1}
            spellCheck={false}
            autoCapitalize="off"
            autoCorrect="off"
          />
        )}
        {menu && (
          <div ref={menuRef} className="kv-menu app-bw-menu" role="menu">
            {menu.fallback ? (
              <div className="kv-menu-item app-bw-menu-note" role="menuitem" aria-disabled="true">
                Use Cmd+V or Ctrl+V to paste
              </div>
            ) : (
              <Button variant="ghost" size="sm" className="app-bw-menu-item" role="menuitem" onClick={pasteClipboard}>
                Paste
              </Button>
            )}
          </div>
        )}
        {status && (
          <div className="app-bw-status" role="status">
            <Text variant="body" tone="muted">
              {status}
            </Text>
          </div>
        )}
      </main>
    </div>
  );
}
