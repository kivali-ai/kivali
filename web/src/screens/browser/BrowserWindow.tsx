import { useEffect, useRef, useState } from 'react';
import { useParams } from 'react-router';
import { apiGet } from '../../api/client';
import type { BrowserState, BrowserWindowEvent, BrowserWindowTab, SubagentMeta, SubagentTranscript } from '../../api/types.gen';
import { Badge, Banner, Text } from '../../ds';
import { asApiError } from '../agent/tabsShared';
import '../../styles/browser.css';

/** Waits before each reconnect, by attempt: 1 s, 2 s, 4 s, then every 10 s. */
export const RECONNECT_MS = [1000, 2000, 4000, 10_000];

function reconnectDelay(attempt: number): number {
  return RECONNECT_MS[Math.min(attempt, RECONNECT_MS.length - 1)] ?? 10_000;
}

/** The same-origin WebSocket URL of a task's browser window, after the page's scheme. */
export function browserSocketUrl(slug: string, id: string, loc: Location = window.location): string {
  const scheme = loc.protocol === 'https:' ? 'wss:' : 'ws:';
  return scheme + '//' + loc.host + '/api/v1/agents/' + encodeURIComponent(slug) + '/subagents/' + encodeURIComponent(id) + '/browser';
}

/** Why the window does not connect to this task, or null when it can. */
function unavailable(meta: SubagentMeta): string | null {
  if (meta.kind !== 'web') return 'This task does not use a browser.';
  if (meta.state !== 'running') return 'The task has ended.';
  return null;
}

function hostOf(url: string): string {
  try {
    return new URL(url).host || url;
  } catch {
    return url;
  }
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
 * A web task's browser window, outside the app frame: the tab the task's browser shows, live, watch only. It
 * reads the task's header once; a running web task gets a WebSocket that carries the tab and the browser's
 * state as text and the frames as JPEGs. A socket that closes without an end event is reopened with backoff
 * while the task still runs.
 */
export function BrowserWindow() {
  const { slug = '', id = '' } = useParams();
  const [meta, setMeta] = useState<SubagentMeta | null>(null);
  const [failure, setFailure] = useState<{ message: string; who: string } | null>(null);
  const [link, setLink] = useState<Link>('connecting');
  const [ended, setEnded] = useState('');
  const [browser, setBrowser] = useState<BrowserState | undefined>(undefined);
  const [tab, setTab] = useState<BrowserWindowTab | null>(null);
  const [framed, setFramed] = useState(false);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const imgRef = useRef<HTMLImageElement>(null);
  const [decodes] = useState(() => typeof createImageBitmap === 'function');

  const path = '/api/v1/agents/' + encodeURIComponent(slug) + '/subagents/' + encodeURIComponent(id);

  useEffect(() => {
    let live = true;
    apiGet<SubagentTranscript>(path).then(
      (t) => {
        if (!live) return;
        setMeta(t.meta);
        setBrowser(t.meta.browser);
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

  const blocked = meta ? unavailable(meta) : null;
  const connect = meta !== null && blocked === null;

  useEffect(() => {
    if (!connect) return;
    const painter = new FramePainter(canvasRef.current, imgRef.current, () => setFramed(true));
    let ws: WebSocket | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let attempt = 0;
    let over = false;

    const finish = (reason: string) => {
      over = true;
      setEnded(reason);
      setLink('ended');
    };

    const open = () => {
      const sock = new WebSocket(browserSocketUrl(slug, id));
      ws = sock;
      sock.binaryType = 'blob';
      sock.onopen = () => {
        attempt = 0;
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
        if (msg.type === 'state') setBrowser(msg.browser);
        else if (msg.type === 'tab' && msg.tab) setTab(msg.tab);
        else if (msg.type === 'end') {
          finish(msg.reason || 'The window has ended.');
          sock.close();
        }
      };
      sock.onclose = () => {
        if (over || ws !== sock) return;
        ws = null;
        setLink('reconnecting');
        timer = setTimeout(retry, reconnectDelay(attempt++));
      };
    };

    // The task may have ended while the socket was down; only a running web task is reconnected.
    const retry = () => {
      apiGet<SubagentTranscript>(path).then(
        (t) => {
          if (over) return;
          const why = unavailable(t.meta);
          if (why) finish(why);
          else open();
        },
        () => {
          if (!over) timer = setTimeout(retry, reconnectDelay(attempt++));
        },
      );
    };

    open();
    return () => {
      over = true;
      clearTimeout(timer);
      if (ws) {
        ws.onclose = null;
        ws.close();
      }
      painter.stop();
    };
  }, [connect, slug, id, path]);

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
  else if (!framed || browser === 'starting') status = 'Starting the browser';

  return (
    <div className="app-bw">
      <header className="app-bw-bar">
        <div className="app-bw-where">
          <Text as="h1" variant="heading" className="app-bw-host">
            {tab ? hostOf(tab.url) : 'Browser'}
          </Text>
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
          <Badge variant="outline">Watch only</Badge>
          {meta?.description && (
            <Text variant="caption" tone="muted" className="app-bw-task">
              {meta.description}
            </Text>
          )}
        </div>
      </header>
      <main className="app-bw-view">
        {connect && (decodes ? <canvas ref={canvasRef} className="app-bw-frame" aria-label="The task's browser" /> : <img ref={imgRef} className="app-bw-frame" alt="The task's browser" />)}
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
