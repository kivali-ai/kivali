import { act, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { fakeIntersectionObserver } from './fakeIntersectionObserver';
import type { FakeObserver } from './fakeIntersectionObserver';
import { useScrollSpy } from './useScrollSpy';

const IDS = ['s-a', 's-b', 's-c', 's-d'];

function Page({ Observer, endId }: { Observer: typeof IntersectionObserver | undefined; endId?: string }) {
  const active = useScrollSpy({ ids: IDS, fallback: 's-b', Observer, ...(endId ? { endId } : {}) });
  return (
    <div>
      <output>{active}</output>
      {IDS.map((id) => (
        <section key={id} id={id} />
      ))}
      <div id="end" />
    </div>
  );
}

function shown(): string {
  return screen.getByRole('status').textContent ?? '';
}

/** Where each section's top is: above the line (passed, left the strip upward), in the strip, or below it. */
function at(spy: FakeObserver, tops: Record<string, 'above' | 'in' | 'below'>) {
  act(() =>
    spy.fire(
      Object.entries(tops).map(([target, where]) => ({
        target,
        isIntersecting: where === 'in',
        top: where === 'below' ? 900 : -400,
      })),
    ),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('useScrollSpy', () => {
  it('reports the fallback until the observer speaks, then the section at the line, down and back up', () => {
    const fake = fakeIntersectionObserver();
    render(<Page Observer={fake.Observer} />);
    expect(shown()).toBe('s-b');
    const spy = fake.instances[0]!;
    expect(spy.targets.map((el) => el.id)).toEqual(IDS);
    expect(spy.options?.rootMargin).toMatch(/^-\d+px 0% -\d+px 0%$/);

    // Top of the page: nothing has reached the line, so the first section wins.
    at(spy, { 's-a': 'below', 's-b': 'below', 's-c': 'below', 's-d': 'below' });
    expect(shown()).toBe('s-a');
    at(spy, { 's-a': 'in' });
    expect(shown()).toBe('s-a');
    at(spy, { 's-a': 'above', 's-b': 'in' });
    expect(shown()).toBe('s-b');
    // In the gap between two sections the last one passed stays.
    at(spy, { 's-b': 'above' });
    expect(shown()).toBe('s-b');
    at(spy, { 's-c': 'in' });
    expect(shown()).toBe('s-c');
    at(spy, { 's-c': 'above', 's-d': 'in' });
    expect(shown()).toBe('s-d');

    // Back up: each section whose top drops below the line hands over to the one above.
    at(spy, { 's-d': 'below', 's-c': 'in' });
    expect(shown()).toBe('s-c');
    at(spy, { 's-c': 'below' });
    expect(shown()).toBe('s-b');
    at(spy, { 's-b': 'below', 's-a': 'in' });
    expect(shown()).toBe('s-a');
    at(spy, { 's-a': 'below' });
    expect(shown()).toBe('s-a');
  });

  it('gives the last section the bottom of the page even when its top never reaches the line', () => {
    const fake = fakeIntersectionObserver();
    render(<Page Observer={fake.Observer} endId="end" />);
    const [spy, tail] = fake.instances;
    expect(tail!.targets.map((el) => el.id)).toEqual(['end']);
    at(spy!, { 's-a': 'above', 's-b': 'in', 's-c': 'below', 's-d': 'below' });
    expect(shown()).toBe('s-b');
    act(() => tail!.fire([{ target: 'end', isIntersecting: true, top: 700 }]));
    expect(shown()).toBe('s-d');
    act(() => tail!.fire([{ target: 'end', isIntersecting: false, top: 1200 }]));
    expect(shown()).toBe('s-b');
  });

  it('falls back to the given section without an IntersectionObserver', () => {
    vi.stubGlobal('IntersectionObserver', undefined);
    render(<Page Observer={undefined} />);
    expect(shown()).toBe('s-b');
  });

  it('uses the window’s observer by default and disconnects on unmount', () => {
    const fake = fakeIntersectionObserver();
    vi.stubGlobal('IntersectionObserver', fake.Observer);
    const { unmount } = render(<Page Observer={undefined} endId="end" />);
    expect(fake.instances).toHaveLength(2);
    expect(fake.instances.every((o) => !o.disconnected)).toBe(true);
    unmount();
    expect(fake.instances.every((o) => o.disconnected)).toBe(true);
  });
});
