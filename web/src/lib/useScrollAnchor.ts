import { useCallback, useEffect, useRef } from 'react';
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
 * scrolling key, a press on the scrollbar) lets go for good, until the next `anchor`. No timers.
 */
export function useScrollAnchor(container: RefObject<Element | null>, Observer?: ResizeCtor): (el: Element, behavior: ScrollBehavior) => void {
  const target = useRef<Element | null>(null);
  const behavior = useRef<ScrollBehavior>('auto');
  const Ctor = Observer ?? defaultObserver();

  useEffect(() => {
    const box = container.current;
    if (!box || !Ctor) return;
    const reapply = () => target.current?.scrollIntoView({ block: 'start', behavior: behavior.current });
    const release = () => {
      target.current = null;
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
    const resize = new Ctor(reapply);
    resize.observe(box);
    const opts = { capture: true, passive: true } as const;
    window.addEventListener('wheel', release, opts);
    window.addEventListener('touchstart', release, opts);
    window.addEventListener('keydown', onKey, opts);
    window.addEventListener('pointerdown', onPointer, opts);
    document.addEventListener('scrollend', arrived);
    return () => {
      resize.disconnect();
      window.removeEventListener('wheel', release, opts);
      window.removeEventListener('touchstart', release, opts);
      window.removeEventListener('keydown', onKey, opts);
      window.removeEventListener('pointerdown', onPointer, opts);
      document.removeEventListener('scrollend', arrived);
    };
  }, [container, Ctor]);

  return useCallback((el: Element, how: ScrollBehavior) => {
    target.current = el;
    behavior.current = how;
    el.scrollIntoView({ block: 'start', behavior: how });
  }, []);
}
