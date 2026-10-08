/**
 * An EventSource wrapper for the server's streams (/org/stream today).
 *
 * - Named-event subscription that survives reconnects: handlers are attached to every new connection.
 * - The browser reconnects a dropped connection by itself (the server's `retry:` sets the backoff);
 *   this wrapper only reports it as `reconnecting`.
 * - Visibility: a hidden tab closes the connection so it does
 *   not hold a connection slot, and becoming visible opens a new one. The server sends a fresh
 *   `snapshot` first on every connect, so nothing is missed while hidden.
 */

export type ConnectionState = 'idle' | 'connecting' | 'open' | 'reconnecting' | 'paused' | 'closed';

/** The slice of EventSource this wrapper uses; tests pass a fake. */
export interface EventSourceLike {
  readonly readyState: number;
  onopen: ((ev: Event) => void) | null;
  onerror: ((ev: Event) => void) | null;
  addEventListener(type: string, listener: (ev: MessageEvent<string>) => void): void;
  close(): void;
}

export interface StreamOptions {
  /** Builds the connection. Defaults to `new EventSource(url)`. */
  open?: (url: string) => EventSourceLike;
  /** The document whose visibility drives open and close. Defaults to the global one. */
  doc?: Pick<Document, 'visibilityState' | 'addEventListener' | 'removeEventListener'>;
}

export type StreamHandler = (data: string, ev: MessageEvent<string>) => void;

export interface EventStream {
  /** Subscribe to a named event; returns the unsubscribe function. */
  on(event: string, handler: StreamHandler): () => void;
  /** Subscribe to connection state changes; the handler is called at once with the current state. */
  onState(handler: (state: ConnectionState) => void): () => void;
  readonly state: ConnectionState;
  /** Open the stream (if the tab is visible) and start following visibility. */
  start(): void;
  /** Close for good and stop following visibility. */
  stop(): void;
}

const CLOSED = 2;

export function createEventStream(url: string, options: StreamOptions = {}): EventStream {
  const open = options.open ?? ((u: string) => new EventSource(u) as EventSourceLike);
  const doc = options.doc ?? document;

  const handlers = new Map<string, Set<StreamHandler>>();
  const stateHandlers = new Set<(s: ConnectionState) => void>();
  let state: ConnectionState = 'idle';
  let source: EventSourceLike | null = null;
  let started = false;

  function setState(next: ConnectionState): void {
    if (next === state) return;
    state = next;
    for (const h of [...stateHandlers]) h(next);
  }

  function attach(es: EventSourceLike, event: string): void {
    es.addEventListener(event, (ev) => {
      for (const h of [...(handlers.get(event) ?? [])]) h(ev.data, ev);
    });
  }

  function connect(): void {
    if (source || !started) return;
    setState('connecting');
    const es = open(url);
    source = es;
    es.onopen = () => setState('open');
    es.onerror = () => {
      // A CLOSED source will not retry on its own; a CONNECTING one is the browser's own backoff.
      setState(es.readyState === CLOSED ? 'closed' : 'reconnecting');
    };
    for (const event of handlers.keys()) attach(es, event);
  }

  function disconnect(): void {
    if (!source) return;
    try {
      source.close();
    } catch {
      // Already closed.
    }
    source = null;
  }

  function onVisibility(): void {
    if (doc.visibilityState === 'visible') {
      connect();
    } else if (source) {
      disconnect();
      setState('paused');
    }
  }

  return {
    on(event, handler) {
      let set = handlers.get(event);
      if (!set) {
        set = new Set();
        handlers.set(event, set);
        if (source) attach(source, event);
      }
      set.add(handler);
      return () => {
        handlers.get(event)?.delete(handler);
      };
    },
    onState(handler) {
      stateHandlers.add(handler);
      handler(state);
      return () => {
        stateHandlers.delete(handler);
      };
    },
    get state() {
      return state;
    },
    start() {
      if (started) return;
      started = true;
      doc.addEventListener('visibilitychange', onVisibility);
      if (doc.visibilityState === 'visible') connect();
      else setState('paused');
    },
    stop() {
      if (!started) return;
      started = false;
      doc.removeEventListener('visibilitychange', onVisibility);
      disconnect();
      setState('idle');
    },
  };
}
