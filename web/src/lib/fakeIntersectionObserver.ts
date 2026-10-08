/**
 * Test support: an IntersectionObserver that reports only what a test tells it to, synchronously. `install()` hands
 * back the constructor (to inject, or to stub onto window) and the list of instances it made.
 */

export interface FakeCrossing {
  /** The observed element, or its id. */
  target: Element | string;
  isIntersecting: boolean;
  /** boundingClientRect.top at the crossing. */
  top: number;
}

export interface FakeObserver {
  readonly options: IntersectionObserverInit | undefined;
  readonly targets: Element[];
  disconnected: boolean;
  /** Deliver one callback with these crossings, as the browser would after a scroll. */
  fire(crossings: FakeCrossing[]): void;
}

export function fakeIntersectionObserver(): { Observer: typeof IntersectionObserver; instances: FakeObserver[] } {
  const instances: FakeObserver[] = [];

  class Fake implements FakeObserver {
    readonly targets: Element[] = [];
    disconnected = false;
    readonly root = null;
    readonly rootMargin: string;
    readonly thresholds: readonly number[] = [0];
    readonly scrollMargin = '0%';

    constructor(
      private readonly callback: IntersectionObserverCallback,
      readonly options: IntersectionObserverInit | undefined,
    ) {
      this.rootMargin = options?.rootMargin ?? '0%';
      instances.push(this);
    }

    observe(el: Element) {
      this.targets.push(el);
    }

    unobserve(el: Element) {
      const at = this.targets.indexOf(el);
      if (at >= 0) this.targets.splice(at, 1);
    }

    disconnect() {
      this.disconnected = true;
      this.targets.length = 0;
    }

    takeRecords(): IntersectionObserverEntry[] {
      return [];
    }

    fire(crossings: FakeCrossing[]) {
      const entries = crossings.map((c) => {
        const target = typeof c.target === 'string' ? document.getElementById(c.target) : c.target;
        if (!target) throw new Error('no element ' + String(c.target));
        const rect = { top: c.top, bottom: c.top + 100, left: 0, right: 100, width: 100, height: 100, x: 0, y: c.top } as DOMRectReadOnly;
        return {
          target,
          isIntersecting: c.isIntersecting,
          intersectionRatio: c.isIntersecting ? 1 : 0,
          boundingClientRect: rect,
          intersectionRect: rect,
          rootBounds: null,
          time: 0,
        } as IntersectionObserverEntry;
      });
      this.callback(entries, this as unknown as IntersectionObserver);
    }
  }

  return { Observer: Fake as unknown as typeof IntersectionObserver, instances };
}
