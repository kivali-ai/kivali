// Shared by the agent's Background, About and Past chats tabs, the task transcript page and Team: a fetch
// hook whose refresh coalesces, router-aware hrefs and the short labels those pages write.
import { useCallback, useEffect, useRef, useState } from 'react';
import { useHref } from 'react-router';
import { ApiError, apiGet } from '../../api/client';
import type { AgentRunState } from '../../ds';
import { shortDate } from '../../lib/time';

export function asApiError(err: unknown): ApiError {
  return err instanceof ApiError ? err : new ApiError(0, 'Something went wrong in Kivali.', 'Reload the page. If it keeps happening, whoever runs this Kivali server can look into it.');
}

export interface Fetched<T> {
  data: T | null;
  /** The last failure; cleared by the next success. Data from an earlier success stays. */
  error: ApiError | null;
  /** Nothing has arrived yet. */
  loading: boolean;
  /**
   * Reads the resource again. A call made while a read is in flight does not start a second one: it marks the
   * resource dirty and one trailing read follows, however many calls came in. The call that started the reads
   * resolves when they finish; a call that only marked the resource dirty resolves at once.
   */
  refresh(): Promise<void>;
  /** Replaces the data without a read (a save that returned the new document). */
  set(next: T): void;
}

/** GET `path` on mount (and when it changes) and again on every `refresh()`, one read at a time. */
export function useFetched<T>(path: string): Fetched<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const generation = useRef(0);
  const inFlight = useRef(false);
  const dirty = useRef(false);

  const run = useCallback(
    async (gen: number) => {
      inFlight.current = true;
      try {
        do {
          dirty.current = false;
          try {
            const next = await apiGet<T>(path);
            if (gen !== generation.current) return;
            setData(next);
            setError(null);
          } catch (err) {
            if (gen !== generation.current) return;
            setError(asApiError(err));
          }
        } while (dirty.current && gen === generation.current);
      } finally {
        if (gen === generation.current) inFlight.current = false;
      }
    },
    [path],
  );

  useEffect(() => {
    const gen = ++generation.current;
    inFlight.current = false;
    dirty.current = false;
    setData(null);
    setError(null);
    void run(gen);
    return () => {
      generation.current++;
    };
  }, [run]);

  const refresh = useCallback(async () => {
    if (inFlight.current) {
      dirty.current = true;
      return;
    }
    await run(generation.current);
  }, [run]);

  const set = useCallback((next: T) => setData(next), []);

  return { data, error, loading: data === null && error === null, refresh, set };
}

/**
 * A router path as an href. The design system's rows render plain anchors; the frame's link interception
 * turns a click on one into a router navigation, so the href carries the app's base path.
 */
export function useAppHref(): (to: string) => string {
  const base = useHref('/').replace(/\/$/, '');
  return (to) => base + to;
}

// ---- Labels ----

const MONTHS_LONG = ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'];

/** "Opus 5.5 · high"; just the model when the effort is unknown. */
export function modelLabel(model: string, effort: string): string {
  return effort ? model + ' · ' + effort : model;
}

/** "~188k tokens", "~800 tokens". */
export function tokensLabel(tokens: number): string {
  return '~' + (tokens >= 1000 ? Math.round(tokens / 1000) + 'k' : String(tokens)) + ' tokens';
}

export function messagesLabel(n: number): string {
  return n + (n === 1 ? ' message' : ' messages');
}

/** "12 to 26 Sept", "18 Aug to 2 Sept", "26 Sept"; empty for a chat with no timestamps. */
export function rangeLabel(fromMs: number, toMs: number, now: number): string {
  if (!fromMs && !toMs) return '';
  const a = new Date(fromMs || toMs);
  const b = new Date(toMs || fromMs);
  const today = new Date(now);
  if (a.toDateString() === b.toDateString()) return shortDate(a, today);
  if (a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth()) return a.getDate() + ' to ' + shortDate(b, today);
  return shortDate(a, today) + ' to ' + shortDate(b, today);
}

/** "September", or "September 2025" outside the current year. */
export function monthLabel(ms: number, now: number): string {
  const d = new Date(ms);
  const name = MONTHS_LONG[d.getMonth()] ?? '';
  return d.getFullYear() === new Date(now).getFullYear() ? name : name + ' ' + d.getFullYear();
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** "just now", "8 minutes ago", "3 hours ago", "yesterday", "2 days ago", then "on 26 Sept". */
export function agoPhrase(iso: string, now: number): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const ago = now - t;
  const plural = (n: number, unit: string) => n + ' ' + unit + (n === 1 ? '' : 's') + ' ago';
  if (ago < MINUTE) return 'just now';
  if (ago < HOUR) return plural(Math.floor(ago / MINUTE), 'minute');
  if (ago < DAY) return plural(Math.floor(ago / HOUR), 'hour');
  if (ago < 2 * DAY) return 'yesterday';
  if (ago < 7 * DAY) return plural(Math.floor(ago / DAY), 'day');
  return 'on ' + shortDate(new Date(t), new Date(now));
}

/** A task's elapsed time: "5:12" while it runs, "3m" or "40s" once it ended, "1h 5m" past an hour. */
export function elapsedLabel(seconds: number, live: boolean): string {
  const s = Math.max(0, Math.floor(seconds));
  if (live && s < 3600) return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
  if (s >= 3600) {
    const m = Math.floor((s % 3600) / 60);
    return Math.floor(s / 3600) + 'h' + (m ? ' ' + m + 'm' : '');
  }
  return s >= 60 ? Math.floor(s / 60) + 'm' : s + 's';
}

/**
 * Memory text as the reader sees it: the durable memory's provenance markers ("[[ep:…]]", "[[stated]]") are
 * bookkeeping for the agent, not words for the person. The editor keeps them.
 */
export function stripProvenance(text: string): string {
  return text.replace(/[ \t]*\[\[(?:ep:[^\]]*|stated)\]\]/g, '');
}

/** How many list items a document holds ("- ", "* ", "+ " or "1. " at the start of a line). */
export function listItemCount(text: string): number {
  return text.split('\n').filter((line) => /^\s*(?:[-*+]\s|\d+[.)]\s)/.test(line)).length;
}

// ---- State vocabularies ----

export type SessionRunState = 'queued' | 'running' | 'done' | 'errored' | 'cancelled';

/** A background session's state as the design system's AgentState draws it (the words already agree). */
export const SESSION_STATE: Record<SessionRunState, AgentRunState> = {
  queued: 'queued',
  running: 'running',
  done: 'done',
  errored: 'errored',
  cancelled: 'cancelled',
};

/** A plan step's state as AgentState draws it: a step that needs help is the "errored" glyph and words. */
export const STEP_STATE: Record<'done' | 'running' | 'idle' | 'needs_help', AgentRunState> = {
  done: 'done',
  running: 'running',
  idle: 'idle',
  needs_help: 'errored',
};
