import { describe, expect, it } from 'vitest';
import { createEventStream } from './sse';
import type { ConnectionState, EventSourceLike } from './sse';

class FakeSource implements EventSourceLike {
  readyState = 0;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  closed = false;
  private listeners = new Map<string, Array<(ev: MessageEvent<string>) => void>>();
  constructor(readonly url: string) {}
  addEventListener(type: string, listener: (ev: MessageEvent<string>) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }
  close() {
    this.closed = true;
    this.readyState = 2;
  }
  open() {
    this.readyState = 1;
    this.onopen?.(new Event('open'));
  }
  send(type: string, data: string) {
    for (const l of this.listeners.get(type) ?? []) l(new MessageEvent(type, { data }));
  }
  fail(readyState: number) {
    this.readyState = readyState;
    this.onerror?.(new Event('error'));
  }
}

class FakeDoc {
  visibilityState: 'visible' | 'hidden' = 'visible';
  private listeners = new Set<() => void>();
  addEventListener(_type: string, l: () => void) {
    this.listeners.add(l);
  }
  removeEventListener(_type: string, l: () => void) {
    this.listeners.delete(l);
  }
  set(state: 'visible' | 'hidden') {
    this.visibilityState = state;
    for (const l of [...this.listeners]) l();
  }
  get listenerCount() {
    return this.listeners.size;
  }
}

function setup(initial: 'visible' | 'hidden' = 'visible') {
  const doc = new FakeDoc();
  doc.visibilityState = initial;
  const sources: FakeSource[] = [];
  const stream = createEventStream('/org/stream', {
    doc: doc as unknown as Document,
    open: (url) => {
      const s = new FakeSource(url);
      sources.push(s);
      return s;
    },
  });
  const states: ConnectionState[] = [];
  stream.onState((s) => states.push(s));
  return { doc, sources, stream, states };
}

describe('createEventStream', () => {
  it('opens on start, reports open, and delivers named events', () => {
    const { sources, stream, states } = setup();
    const got: string[] = [];
    stream.on('snapshot', (data) => got.push(data));
    stream.start();
    expect(sources).toHaveLength(1);
    expect(sources[0]?.url).toBe('/org/stream');
    sources[0]?.open();
    sources[0]?.send('snapshot', '{"a":1}');
    sources[0]?.send('other', 'ignored');
    expect(got).toEqual(['{"a":1}']);
    expect(states).toEqual(['idle', 'connecting', 'open']);
  });

  it('attaches a handler added after the connection opened', () => {
    const { sources, stream } = setup();
    stream.start();
    const got: string[] = [];
    stream.on('late', (d) => got.push(d));
    sources[0]?.send('late', 'x');
    expect(got).toEqual(['x']);
  });

  it('unsubscribes', () => {
    const { sources, stream } = setup();
    const got: string[] = [];
    const off = stream.on('snapshot', (d) => got.push(d));
    stream.start();
    off();
    sources[0]?.send('snapshot', 'x');
    expect(got).toEqual([]);
  });

  it('reports reconnecting while the browser retries and closed when it gave up', () => {
    const { sources, stream, states } = setup();
    stream.start();
    sources[0]?.open();
    sources[0]?.fail(0);
    expect(stream.state).toBe('reconnecting');
    sources[0]?.open();
    expect(stream.state).toBe('open');
    sources[0]?.fail(2);
    expect(stream.state).toBe('closed');
    expect(states).toContain('reconnecting');
  });

  it('closes when the tab is hidden and opens a new connection when it is visible again', () => {
    const { doc, sources, stream } = setup();
    const got: string[] = [];
    stream.on('snapshot', (d) => got.push(d));
    stream.start();
    sources[0]?.open();

    doc.set('hidden');
    expect(sources[0]?.closed).toBe(true);
    expect(stream.state).toBe('paused');

    doc.set('visible');
    expect(sources).toHaveLength(2);
    sources[1]?.open();
    sources[1]?.send('snapshot', 'fresh');
    expect(got).toEqual(['fresh']);
    expect(stream.state).toBe('open');
  });

  it('does not open while hidden, and opens when it becomes visible', () => {
    const { doc, sources, stream } = setup('hidden');
    stream.start();
    expect(sources).toHaveLength(0);
    expect(stream.state).toBe('paused');
    doc.set('visible');
    expect(sources).toHaveLength(1);
  });

  it('never holds two connections', () => {
    const { doc, sources, stream } = setup();
    stream.start();
    doc.set('visible');
    doc.set('visible');
    expect(sources).toHaveLength(1);
  });

  it('stop closes for good and stops following visibility', () => {
    const { doc, sources, stream } = setup();
    stream.start();
    stream.stop();
    expect(sources[0]?.closed).toBe(true);
    expect(stream.state).toBe('idle');
    expect(doc.listenerCount).toBe(0);
    doc.set('hidden');
    doc.set('visible');
    expect(sources).toHaveLength(1);
  });
});
