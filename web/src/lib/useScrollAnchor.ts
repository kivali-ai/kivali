import { useCallback, useEffect, useRef, useState } from 'react';
import type { RefObject } from 'react';

type ResizeCtor = typeof ResizeObserver;

/** Keys that scroll the page (or move focus, which can): pressing one hands the scroll back to the reader. */
const SCROLL_KEYS = new Set(['ArrowUp', 'ArrowDown', 'PageUp', 'PageDown', 'Home', 'End', ' ', 'Spacebar', 'Tab']);

function defaultObserver(): ResizeCtor | undefined {
  return typeof window !== 'undefined' && typeof window.ResizeObserver === 'function' ? window.ResizeObserver : undefined;
}

/** A pointer pressed on the page's own scrollbar (past the root's client box) or middle-clicked into autoscroll. */
function onScrollbar(ev: PointerEvent): boolean {
  const root = document.documentElement;
  return ev.button === 1 || ev.clientX >= root.clientWidth || ev.clientY >= root.clientHeight;
}

/**
 * Keeps a scrolled-to element at the top while the content around it is still settling. `anchor(el, behavior)`
 * scrolls `el` to its block start and remembers it; each time `container` changes size (a list above it loaded, a
 * page that was too short to reach it grew), the scroll is applied again: with the same behavior while that first
 * scroll travels, instantly once it has ended. The first sign the reader is scrolling themselves (wheel, touch, a
 * scrolling key, a press on the scrollbar) lets go for good, until the next `anchor`. No timers. `anchored` is the
 * element held, as state, and null once let go. A press on an overlay scrollbar, which takes no layout space, is
 * not seen, nor is find-in-page.
 */
export function useScrollAnchor(
  container: RefObject<Element | null>,
  Observer?: ResizeCtor,
): { anchor: (el: Element, behavior: ScrollBehavior) => void; anchored: Element | null } {
  const target = useRef<Element | null>(null);
  const behavior = useRef<ScrollBehavior>('auto');
  const [anchored, setAnchored] = useState<Element | null>(null);
  const Ctor = Observer ?? defaultObserver();

  useEffect(() => {
    const box = container.current;
    const reapply = () => target.current?.scrollIntoView({ block: 'start', behavior: behavior.current });
    const release = () => {
      if (target.current === null) return;
      target.current = null;
      setAnchored(null);
    };
    const onKey = (ev: KeyboardEvent) => {
      if (SCROLL_KEYS.has(ev.key)) release();
    };
    const onPointer = (ev: PointerEvent) => {
      if (onScrollbar(ev)) release();
    };
    const arrived = () => {
      behavior.current = 'auto';
    };
    // Letting go never depends on a ResizeObserver: without one the element is still held, and still released.
    const resize = box && Ctor ? new Ctor(reapply) : null;
    if (box) resize?.observe(box);
    const opts = { capture: true, passive: true } as const;
    window.addEventListener('wheel', release, opts);
    window.addEventListener('touchstart', release, opts);
    window.addEventListener('keydown', onKey, opts);
    window.addEventListener('pointerdown', onPointer, opts);
    document.addEventListener('scrollend', arrived);
    return () => {
      resize?.disconnect();
      window.removeEventListener('wheel', release, opts);
      window.removeEventListener('touchstart', release, opts);
      window.removeEventListener('keydown', onKey, opts);
      window.removeEventListener('pointerdown', onPointer, opts);
      document.removeEventListener('scrollend', arrived);
    };
  }, [container, Ctor]);

  const anchor = useCallback((el: Element, how: ScrollBehavior) => {
    target.current = el;
    behavior.current = how;
    setAnchored(el);
    el.scrollIntoView({ block: 'start', behavior: how });
  }, []);
  return { anchor, anchored };
}
