import { useCallback, useEffect, useMemo, useRef } from 'react';
import type { Dispatch } from 'react';
import type { EventStream } from '../../api/sse';
import type { TranscriptAction } from '../../state/transcript';
import type { ChatDeps } from './chatDeps';

/** Every event /agents/{slug}/stream sends that the chat reads. */
export const AGENT_STREAM_EVENTS = [
  'delta',
  'thinking',
  'tool_use_start',
  'tool_input_delta',
  'tool_use',
  'tool_result',
  'doc_published',
  'file_shared',
  'subagent_started',
  'subagent_progress',
  'subagent_activity',
  'subagent_completed',
  'chat_message',
  'chat_marker',
  'wake_update',
  'chat_fill',
  'done',
  'rotation_done',
  'error',
  'pending_message',
  'pending_offered',
  'pending_delivered',
  'pending_deleted',
] as const;

/** A rotation's `done` is followed by `rotation_done`; if that never comes, the chat is fetched anyway. */
export const ROTATION_FALLBACK_MS = 15_000;

export interface AgentStreamControl {
  /** Opens the stream. `force` replaces one that is open (it may have died while the tab was hidden). */
  open(force?: boolean): void;
  close(): void;
  isOpen(): boolean;
}

function parse(data: string | undefined): unknown {
  if (data === undefined || data === '') return undefined;
  try {
    return JSON.parse(data) as unknown;
  } catch {
    return undefined;
  }
}

/**
 * The agent's event stream, one at a time. The server answers 204 when nothing is running, which closes the
 * EventSource with an error: that is reported as `stream_closed`, never as a failure. `done` and the
 * server's own `error` event end the stream; a rotation keeps it open for `rotation_done`, whichever of
 * the two ended the turn that folded the chat.
 */
export function useAgentStream(slug: string, dispatch: Dispatch<TranscriptAction>, deps: ChatDeps): AgentStreamControl {
  const current = useRef<EventStream | null>(null);
  const rotationTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const depsRef = useRef(deps);
  depsRef.current = deps;

  const clearRotation = useCallback(() => {
    if (rotationTimer.current) clearTimeout(rotationTimer.current);
    rotationTimer.current = null;
  }, []);

  const close = useCallback(() => {
    const s = current.current;
    current.current = null;
    s?.stop();
  }, []);

  const open = useCallback(
    (force = false) => {
      if (current.current && !force) return;
      close();
      const s = depsRef.current.openStream('/agents/' + encodeURIComponent(slug) + '/stream');
      current.current = s;
      const now = () => depsRef.current.now();
      for (const name of AGENT_STREAM_EVENTS) {
        s.on(name, (data) => {
          if (current.current !== s) return;
          const payload = parse(data);
          // A transport failure also arrives as "error", with no data: the connection state covers it.
          if (name === 'error' && payload === undefined) return;
          if (payload === undefined && name !== 'rotation_done') return;
          dispatch({ type: 'event', name, data: payload ?? {}, at: now() });
          if (name === 'done' || name === 'error') {
            const rotation = (payload as { rotation?: unknown }).rotation === true;
            if (!rotation) {
              close();
              return;
            }
            clearRotation();
            rotationTimer.current = setTimeout(() => {
              rotationTimer.current = null;
              if (current.current === s) close();
              dispatch({ type: 'event', name: 'rotation_done', data: {}, at: now() });
            }, ROTATION_FALLBACK_MS);
          } else if (name === 'rotation_done') {
            clearRotation();
            close();
          }
        });
      }
      // Whether this connection ever opened: a 204 probe closes without opening, a mid-turn drop after.
      let opened = false;
      s.onState((state) => {
        if (current.current !== s) return;
        if (state === 'open') {
          opened = true;
          dispatch({ type: 'stream_open', at: now() });
        } else if (state === 'closed') {
          current.current = null;
          s.stop();
          dispatch({ type: 'stream_closed', wasOpen: opened });
        }
      });
      s.start();
    },
    [slug, dispatch, close, clearRotation],
  );

  useEffect(
    () => () => {
      clearRotation();
      close();
    },
    [slug, close, clearRotation],
  );

  return useMemo(() => ({ open, close, isOpen: () => current.current !== null }), [open, close]);
}
