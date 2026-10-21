import { useEffect, useState } from 'react';

type ObserverCtor = typeof IntersectionObserver;

export interface ScrollSpyOptions {
  /** Element ids of the sections, in page order. */
  ids: readonly string[];
  /** The id to report until the observer has spoken, and always when there is no IntersectionObserver. */
  fallback: string;
  /** Height of a sticky header above the sections, in px. The line sits this far down plus the sections' scroll-margin-top. */
  offset?: number;
  /** Id of an element at the very end of the page. While it is on screen the last section wins. */
  endId?: string;
  /**
   * Id of the section the page scrolled to on purpose, until the reader scrolls themselves. While `endId` is on
   * screen it wins over the last section unless a section after it has passed the line: the page may have been too
   * short to bring it up to the line, and it is still the one asked for. An id that is not one of `ids` is ignored.
   */
  prefer?: string | undefined;
  /** Off: report the fallback and observe nothing. */
  enabled?: boolean;
  /** The IntersectionObserver constructor; tests pass a fake. Defaults to the window's. */
  Observer?: ObserverCtor | undefined;
}

/** Slack below the line so a section scrolled to its scroll-margin by scrollIntoView counts as reached. */
const SLACK = 2;

function defaultObserver(): ObserverCtor | undefined {
  return typeof window !== 'undefined' && typeof window.IntersectionObserver === 'function' ? window.IntersectionObserver : undefined;
}

function scrollMargin(el: Element): number {
  const value = parseFloat(window.getComputedStyle(el).scrollMarginTop);
  return Number.isFinite(value) ? value : 0;
}

/**
 * Which section the reader is in: the last one whose top has passed a line near the top of the viewport. The line
 * is `offset` plus the sections' scroll-margin-top (where scrollIntoView leaves a section), so a section scrolled to
 * is the one reported. Above the first section the first wins; while `endId` is on screen (the page cannot scroll
 * further) the last wins even if its top never reaches the line, or `prefer` when the page scrolled to it and the
 * reader has not scrolled on past it.
 *
 * One IntersectionObserver watches a 1px strip at the line: a section enters or leaves it exactly when its top or
 * bottom crosses the line, and the entry's rectangle says which. No scroll listeners, no timers. A window resize
 * rebuilds the strip, whose bottom margin is in px. The observers only record what they see (the section last
 * passed, whether the end is on screen); which section to report is decided from that at render, so a change of
 * `prefer` rebuilds nothing.
 */
export function useScrollSpy({ ids, fallback, offset = 0, endId, prefer, enabled = true, Observer }: ScrollSpyOptions): string {
  // The last section whose top has passed the line (the first when none has); null until the observer has spoken.
  const [reached, setReached] = useState<string | null>(null);
  const [atEnd, setAtEnd] = useState(false);
  const [height, setHeight] = useState(() => (typeof window === 'undefined' ? 0 : window.innerHeight));
  const Ctor = Observer ?? defaultObserver();
  const key = ids.join('\n');

  useEffect(() => {
    if (!enabled || !Ctor) return;
    const onResize = () => setHeight(window.innerHeight);
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
  }, [enabled, Ctor]);

  useEffect(() => {
    if (!enabled || !Ctor) return;
    const order = key === '' ? [] : key.split('\n');
    const elements = order.map((id) => document.getElementById(id)).filter((el): el is HTMLElement => el !== null);
    if (elements.length === 0) return;

    const idOf = new Map<Element, string>(elements.map((el) => [el, el.id]));
    const passed = new Map<string, boolean>();

    const line = offset + scrollMargin(elements[0]!) + SLACK;
    const below = Math.max(0, height - line - 1);
    const spy = new Ctor(
      (entries) => {
        for (const entry of entries) {
          const id = idOf.get(entry.target);
          if (id === undefined) continue;
          // Crossing the strip: inside means the top is above the line; outside, the rectangle says above or below.
          passed.set(id, entry.isIntersecting || entry.boundingClientRect.top < line);
        }
        let current = order[0] ?? null;
        for (const id of order) if (passed.get(id)) current = id;
        setReached(current);
      },
      { rootMargin: `-${line}px 0% -${below}px 0%`, threshold: 0 },
    );
    for (const el of elements) spy.observe(el);

    const end = endId ? document.getElementById(endId) : null;
    const tail = end
      ? new Ctor((entries) => {
          const last = entries[entries.length - 1];
          if (last) setAtEnd(last.isIntersecting);
        })
      : null;
    if (end && tail) tail.observe(end);

    return () => {
      spy.disconnect();
      tail?.disconnect();
    };
  }, [enabled, Ctor, key, offset, endId, height]);

  if (!enabled || !Ctor) return fallback;
  if (atEnd) {
    // The bottom of the page belongs to the last section, unless the page scrolled to one on purpose and the reader
    // has not gone on past it.
    const asked = prefer !== undefined && ids.includes(prefer) ? prefer : null;
    const past = asked !== null && reached !== null && ids.indexOf(reached) > ids.indexOf(asked);
    return asked !== null && !past ? asked : (ids[ids.length - 1] ?? fallback);
  }
  return reached ?? fallback;
}
