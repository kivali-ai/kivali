import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeStream } from '../../app/test-utils';
import type { TranscriptAction } from '../../state/transcript';
import type { ChatDeps } from './chatDeps';
import { ROTATION_FALLBACK_MS, useAgentStream } from './useAgentStream';

const NOW = 1789923700000;

function follow() {
  const streams: FakeStream[] = [];
  const actions: TranscriptAction[] = [];
  const deps: ChatDeps = {
    openStream() {
      const s = new FakeStream();
      streams.push(s);
      return s;
    },
    now: () => NOW,
  };
  const hook = renderHook(() => useAgentStream('engineering-lead', (a) => actions.push(a), deps));
  act(() => hook.result.current.open());
  const stream = streams[0];
  if (!stream) throw new Error('no stream was opened');
  const events = () => actions.filter((a) => a.type === 'event').map((a) => a.name);
  return { hook, stream, events, control: () => hook.result.current };
}

beforeEach(() => {
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
});

describe('useAgentStream: the end of a turn', () => {
  it.each(['done', 'error'])('%s without a rotation closes the stream', (name) => {
    const { stream, events, control } = follow();
    act(() => stream.emit(name, { error: 'Boom.', waiting_tasks: 0, rotation: false }));
    expect(events()).toEqual([name]);
    expect(stream.stopped).toBe(true);
    expect(control().isOpen()).toBe(false);
    // And nothing is left waiting to fire.
    act(() => vi.advanceTimersByTime(ROTATION_FALLBACK_MS * 2));
    expect(events()).toEqual([name]);
  });

  it.each(['done', 'error'])('%s with a rotation holds the stream for rotation_done, which closes it', (name) => {
    const { stream, events, control } = follow();
    act(() => stream.emit(name, { error: 'Boom.', waiting_tasks: 0, rotation: true }));
    expect(stream.stopped).toBe(false);
    expect(control().isOpen()).toBe(true);
    act(() => vi.advanceTimersByTime(ROTATION_FALLBACK_MS - 1));
    expect(events()).toEqual([name]);
    act(() => stream.emit('rotation_done', {}));
    expect(events()).toEqual([name, 'rotation_done']);
    expect(stream.stopped).toBe(true);
    // The fallback was cancelled: rotation_done is said once.
    act(() => vi.advanceTimersByTime(ROTATION_FALLBACK_MS * 2));
    expect(events()).toEqual([name, 'rotation_done']);
  });

  it('says rotation_done itself when the server never does, so the page is not left on the archived chat', () => {
    const { stream, events } = follow();
    act(() => stream.emit('done', { waiting_tasks: 0, rotation: true }));
    act(() => vi.advanceTimersByTime(ROTATION_FALLBACK_MS - 1));
    expect(events()).toEqual(['done']);
    act(() => vi.advanceTimersByTime(1));
    expect(events()).toEqual(['done', 'rotation_done']);
    expect(stream.stopped).toBe(true);
  });

  it('still says it when the connection dropped while it was holding', () => {
    const { stream, events, hook } = follow();
    const actions = () => hook.result.current;
    act(() => stream.emit('done', { waiting_tasks: 0, rotation: true }));
    act(() => stream.setState('closed'));
    expect(actions().isOpen()).toBe(false);
    act(() => vi.advanceTimersByTime(ROTATION_FALLBACK_MS));
    expect(events()).toEqual(['done', 'rotation_done']);
  });

  it('drops the fallback with the page', () => {
    const { stream, events, hook } = follow();
    act(() => stream.emit('done', { waiting_tasks: 0, rotation: true }));
    hook.unmount();
    vi.advanceTimersByTime(ROTATION_FALLBACK_MS * 2);
    expect(events()).toEqual(['done']);
    expect(stream.stopped).toBe(true);
  });
});
