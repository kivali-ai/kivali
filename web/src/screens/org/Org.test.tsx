import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, apiDelete, apiGet, apiGetBlob, apiPost, apiPostForm, apiPut } from '../../api/client';
import type { Handbook, Me, Network, Org as OrgData, ProjectFiles, Skills, Usage } from '../../api/types.gen';
import { FrameChromeContext } from '../../app/chrome';
import type { FrameChrome } from '../../app/chrome';
import { TooltipProvider } from '../../ds';
import { fakeIntersectionObserver } from '../../lib/fakeIntersectionObserver';
import type { FakeObserver } from '../../lib/fakeIntersectionObserver';
import { pickFiles } from '../../lib/pickFiles';
import { goMe, goSnapshot } from '../../state/fixtures';
import { initialOrgState, reduceSnapshot } from '../../state/org';
import type { OrgContextValue } from '../../state/OrgProvider';
import { aboutLine, versionLabel } from './About';
import { BACKUP_DEBOUNCE_MS } from './Backup';
import { Org, headerMeta } from './OrgPage';
import { goHandbook, goDowngrade, goFiles, goNetwork, goOrg, goSkills, goUsage } from './orgFixtures';
import { downgradeOf } from './Skills';
import { compact, unpricedNote } from './Usage';

// ---- Seams: the API client (a routed fake), the org provider (a tiny store) and the file picker ----

vi.mock('../../api/client', async (importOriginal) => {
  const real = await importOriginal<typeof import('../../api/client')>();
  return {
    ...real,
    apiGet: vi.fn(),
    apiPost: vi.fn(),
    apiPut: vi.fn(),
    apiDelete: vi.fn(),
    apiPostForm: vi.fn(),
    apiGetBlob: vi.fn(),
  };
});

vi.mock('../../lib/pickFiles', () => ({ pickFiles: vi.fn() }));

const orgStore = vi.hoisted(() => {
  let value: unknown = null;
  const listeners = new Set<() => void>();
  return {
    get: () => value,
    set(next: unknown) {
      value = next;
      for (const l of listeners) l();
    },
    subscribe(l: () => void) {
      listeners.add(l);
      return () => void listeners.delete(l);
    },
  };
});

vi.mock('../../state/OrgProvider', async () => {
  const { useSyncExternalStore } = await import('react');
  return { useOrg: () => useSyncExternalStore(orgStore.subscribe, orgStore.get), usePersonName: () => 'You' };
});

// ---- Harness ----

interface Call {
  method: string;
  url: string;
  body: unknown;
}

/** A route answers with a value, or throws when the value (or what a function returns) is an Error. */
type Route = unknown;

let routes: Record<string, Route> = {};
let calls: Call[] = [];
const refreshMe = vi.fn(async () => {});

function answer(method: string, url: string, body: unknown): Promise<unknown> {
  calls.push({ method, url, body });
  const hit = routes[method + ' ' + url];
  if (hit === undefined) return Promise.reject(new ApiError(404, 'not found', 'Nobody'));
  const value = typeof hit === 'function' ? (hit as (b: unknown) => unknown)(body) : hit;
  if (value instanceof Error) return Promise.reject(value);
  return Promise.resolve(value instanceof Blob ? value : structuredClone(value));
}

function fail(status: number, error: string, who: string, body?: unknown): ApiError {
  return new ApiError(status, error, who, body ?? { error, who });
}

function wireClient() {
  vi.mocked(apiGet).mockImplementation((path) => answer('GET', path, undefined) as never);
  vi.mocked(apiPost).mockImplementation((path, body) => answer('POST', path, body) as never);
  vi.mocked(apiPut).mockImplementation((path, body) => answer('PUT', path, body) as never);
  vi.mocked(apiDelete).mockImplementation((path) => answer('DELETE', path, undefined) as never);
  vi.mocked(apiPostForm).mockImplementation((path, form) => answer('POST', path, form) as never);
  vi.mocked(apiGetBlob).mockImplementation((path) => answer('GET', path, undefined).then((blob) => ({ blob: blob as Blob, filename: null })));
}

function defaults(): Record<string, Route> {
  return {
    'GET /api/v1/org/usage': goUsage,
    'GET /api/v1/org': goOrg,
    'GET /api/v1/org/handbook': goHandbook,
    'GET /api/v1/org/files': goFiles,
    'GET /api/v1/org/skills': goSkills,
    'GET /api/v1/org/network': goNetwork,
    'GET /api/v1/setup': { credential: { ready: true, provider: 'claude', present: true, who: 'owner@example.com', billing: 'Claude Max', guidance: '' }, restore_available: false },
  };
}

function setOrg(me: Me | null) {
  const value: Partial<OrgContextValue> = { me, org: reduceSnapshot(initialOrgState, goSnapshot), refreshMe };
  act(() => orgStore.set(value));
}

function media(desktopWidth: boolean, reducedMotion = false) {
  window.matchMedia = ((query: string) =>
    ({
      matches: (desktopWidth && query.includes('min-width')) || (reducedMotion && query.includes('prefers-reduced-motion: reduce')),
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }) as unknown as MediaQueryList) as typeof window.matchMedia;
}

/** Lets every settled promise run and React commit what they set. No timers involved. */
async function flush() {
  await act(async () => {
    for (let i = 0; i < 20; i++) await Promise.resolve();
  });
}

interface Harness {
  chrome: FrameChrome[];
  router: ReturnType<typeof createMemoryRouter>;
  unmount(): void;
}

async function renderOrg(
  path: string,
  opts: { desktop?: boolean; reducedMotion?: boolean; routes?: Record<string, Route>; me?: Me | null } = {},
): Promise<Harness> {
  media(opts.desktop ?? false, opts.reducedMotion ?? false);
  routes = { ...defaults(), ...opts.routes };
  setOrg(opts.me === undefined ? goMe : opts.me);
  const chrome: FrameChrome[] = [];
  const control = { setChrome: (c: FrameChrome | null) => void (c && chrome.push(c)) };
  const router = createMemoryRouter(
    [
      { path: '/org', element: <Org /> },
      { path: '/org/:section', element: <Org /> },
    ],
    { basename: '/', initialEntries: [path] },
  );
  const { unmount } = render(
    <TooltipProvider>
      <FrameChromeContext.Provider value={control}>
        <RouterProvider router={router} />
      </FrameChromeContext.Provider>
    </TooltipProvider>,
  );
  await flush();
  return { chrome, router, unmount };
}

function called(method: string, url: string): boolean {
  return calls.some((c) => c.method === method && c.url === url);
}

function lastBody(method: string, url: string): unknown {
  return calls.filter((c) => c.method === method && c.url === url).at(-1)?.body;
}

function fileInput(): HTMLInputElement {
  const el = document.querySelector('input[type="file"]');
  if (!(el instanceof HTMLInputElement)) throw new Error('no file input');
  return el;
}

async function drop(file: File) {
  fireEvent.change(fileInput(), { target: { files: [file] } });
  await flush();
}

async function click(el: HTMLElement) {
  fireEvent.click(el);
  await flush();
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date('2026-09-28T12:00:00Z'));
  calls = [];
  refreshMe.mockClear();
  wireClient();
  vi.mocked(pickFiles).mockReset();
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// ---- Layout ----

describe('Org layout', () => {
  it('is a list of sub-pages on phone, with today’s spend above it', async () => {
    await renderOrg('/org');
    expect(screen.getByText('$4.25')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Skills/ })).toHaveAttribute('href', '/org/skills');
    for (const name of ['Organization', 'Handbook', 'Project files', 'Network', 'Backup and restore', 'About']) {
      expect(screen.getByRole('link', { name: new RegExp(name) })).toBeInTheDocument();
    }
    expect(screen.getByRole('link', { name: /About/ })).toHaveTextContent('Kivali 0.15.0');
    expect(screen.queryByRole('heading', { level: 2 })).toBeNull();
    // The phone header carries the page's h1.
    expect(screen.queryByRole('heading', { level: 1 })).toBeNull();
  });

  it('opens one section full screen with a back link to Org on phone', async () => {
    const h = await renderOrg('/org/skills');
    expect(screen.getByRole('switch', { name: 'Enable quote-check' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { level: 2, name: 'Usage' })).toBeNull();
    expect(h.chrome.at(-1)).toEqual({ title: 'Skills', back: { to: '/org', label: 'Org' } });
    expect(called('GET', '/api/v1/org/network')).toBe(false);
  });

  it('is one sectioned page with a left index on desktop, and the section route scrolls to it', async () => {
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    await renderOrg('/org/skills', { desktop: true });
    const names = ['Usage', 'Organization', 'Handbook', 'Project files', 'Skills', 'Network', 'Backup and restore', 'About'];
    expect(screen.getAllByRole('heading', { level: 2 }).map((el) => el.textContent)).toEqual(names);
    const index = screen.getByRole('navigation', { name: 'Org sections' });
    expect(within(index).getAllByRole('link').map((el) => el.textContent)).toEqual(names);
    expect(within(index).getByRole('link', { name: 'Skills' })).toHaveAttribute('aria-current', 'page');
    expect(within(index).getByRole('link', { name: 'Skills' })).toHaveAttribute('href', '/org/skills');
    expect(scroll).toHaveBeenCalled();
    expect(scroll.mock.instances[0]).toBe(document.getElementById('org-skills'));
  });

  it('heads the desktop page with the org’s mark, its name as the one h1, and who is in it', async () => {
    await renderOrg('/org', { desktop: true });
    const h1s = screen.getAllByRole('heading', { level: 1 });
    expect(h1s).toHaveLength(1);
    expect(h1s[0]).toHaveTextContent('Plainsong');
    const header = h1s[0]!.closest('.app-org-header') as HTMLElement;
    expect(within(header).getByRole('img', { name: 'Plainsong' }).querySelector('img')).toHaveAttribute('src', '/branding/icon-180.png');
    expect(within(header).getByText(headerMeta(goSnapshot.agents.length))).toBeInTheDocument();
  });

  it('names an org that has none yet and counts agents in words', async () => {
    await renderOrg('/org', { desktop: true, me: { ...goMe, org: { name: '', has_logo: false, owner_name: '' } } });
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Your org');
    expect(headerMeta(6)).toBe('Org settings · you and 6 agents');
    expect(headerMeta(1)).toBe('Org settings · you and 1 agent');
  });
});

// ---- Desktop index: the highlight follows the scroll ----

describe('Org index scroll spy', () => {
  type Where = 'above' | 'in' | 'below';

  function install() {
    const fake = fakeIntersectionObserver();
    vi.stubGlobal('IntersectionObserver', fake.Observer);
    const live = () => fake.instances.filter((o) => !o.disconnected);
    return {
      fake,
      /** The strip observer on the nine sections. */
      spy: () => live().find((o) => o.targets.length > 1)!,
      /** The observer on the end-of-page marker. */
      tail: () => live().find((o) => o.targets.length === 1 && o.targets[0]!.id === 'org-end')!,
    };
  }

  /** Report section tops against the line, keyed by section key. */
  function at(spy: FakeObserver, tops: Record<string, Where>) {
    act(() =>
      spy.fire(
        Object.entries(tops).map(([key, where]) => ({ target: 'org-' + key, isIntersecting: where === 'in', top: where === 'below' ? 900 : -400 })),
      ),
    );
  }

  /** The one index item marked current (and that there is exactly one). */
  function current(): string {
    const index = screen.getByRole('navigation', { name: 'Org sections' });
    const marked = within(index)
      .getAllByRole('link')
      .filter((el) => el.getAttribute('aria-current') === 'page');
    expect(marked).toHaveLength(1);
    return marked[0]!.textContent ?? '';
  }

  function indexLink(name: string): HTMLElement {
    return within(screen.getByRole('navigation', { name: 'Org sections' })).getByRole('link', { name });
  }

  const ALL_BELOW: Record<string, Where> = {
    usage: 'below',
    organization: 'below',
    handbook: 'below',
    files: 'below',
    skills: 'below',
    network: 'below',
    backup: 'below',
    about: 'below',
  };

  beforeEach(() => {
    Element.prototype.scrollIntoView = vi.fn();
  });

  it('starts on the route’s section, then moves down and back up with the scroll without touching the URL', async () => {
    const io = install();
    const h = await renderOrg('/org/files', { desktop: true });
    expect(current()).toBe('Project files');
    expect(io.spy().targets.map((el) => el.id)).toEqual(['usage', 'organization', 'handbook', 'files', 'skills', 'network', 'backup', 'about'].map((k) => 'org-' + k));

    at(io.spy(), ALL_BELOW);
    expect(current()).toBe('Usage');
    at(io.spy(), { usage: 'in' });
    expect(current()).toBe('Usage');
    at(io.spy(), { usage: 'above', organization: 'in' });
    expect(current()).toBe('Organization');
    at(io.spy(), { organization: 'above' });
    expect(current()).toBe('Organization');
    at(io.spy(), { handbook: 'in' });
    expect(current()).toBe('Handbook');
    at(io.spy(), { handbook: 'above', files: 'in' });
    expect(current()).toBe('Project files');

    at(io.spy(), { files: 'below', handbook: 'in' });
    expect(current()).toBe('Handbook');
    at(io.spy(), { handbook: 'below' });
    expect(current()).toBe('Organization');
    at(io.spy(), { organization: 'below', usage: 'in' });
    expect(current()).toBe('Usage');
    at(io.spy(), { usage: 'below' });
    expect(current()).toBe('Usage');

    expect(h.router.state.location.pathname).toBe('/org/files');
    expect(h.router.state.historyAction).toBe('POP');
  });

  it('gives About the bottom of the page even when its top never reaches the line', async () => {
    const io = install();
    await renderOrg('/org', { desktop: true });
    at(io.spy(), { ...ALL_BELOW, usage: 'above', organization: 'above', handbook: 'above', files: 'above', skills: 'above', network: 'above', backup: 'in' });
    expect(current()).toBe('Backup and restore');
    act(() => io.tail().fire([{ target: 'org-end', isIntersecting: true, top: 700 }]));
    expect(current()).toBe('About');
    act(() => io.tail().fire([{ target: 'org-end', isIntersecting: false, top: 1100 }]));
    expect(current()).toBe('Backup and restore');
  });

  it('navigates and scrolls on a click, holding the clicked item through the sections passed on the way', async () => {
    const io = install();
    const h = await renderOrg('/org', { desktop: true });
    at(io.spy(), { ...ALL_BELOW, usage: 'in' });
    expect(current()).toBe('Usage');
    const scroll = vi.mocked(Element.prototype.scrollIntoView);
    scroll.mockClear();

    fireEvent.click(indexLink('Network'));
    await flush();
    expect(h.router.state.location.pathname).toBe('/org/network');
    // One scroll, the smooth one the click started; the route change does not jump over it.
    expect(scroll).toHaveBeenCalledTimes(1);
    expect(scroll.mock.instances[0]).toBe(document.getElementById('org-network'));
    expect(scroll).toHaveBeenCalledWith({ block: 'start', behavior: 'smooth' });
    expect(current()).toBe('Network');

    at(io.spy(), { usage: 'above', organization: 'in' });
    expect(current()).toBe('Network');
    at(io.spy(), { organization: 'above', handbook: 'in' });
    expect(current()).toBe('Network');
    at(io.spy(), { handbook: 'above', files: 'above', skills: 'in' });
    expect(current()).toBe('Network');
    // Arrived: the spy has it, and from here it follows the scroll again.
    at(io.spy(), { skills: 'above', network: 'in' });
    expect(current()).toBe('Network');
    at(io.spy(), { network: 'below', skills: 'in' });
    expect(current()).toBe('Skills');
    expect(h.router.state.location.pathname).toBe('/org/network');
  });

  it('hands the highlight back to the spy when the scroll ends short of the clicked section', async () => {
    const io = install();
    await renderOrg('/org', { desktop: true });
    at(io.spy(), { ...ALL_BELOW, usage: 'above', organization: 'above', handbook: 'above', files: 'above', skills: 'above', network: 'in' });
    expect(current()).toBe('Network');
    fireEvent.click(indexLink('Backup and restore'));
    await flush();
    expect(current()).toBe('Backup and restore');
    act(() => io.tail().fire([{ target: 'org-end', isIntersecting: true, top: 700 }]));
    expect(current()).toBe('Backup and restore');
    act(() => void document.dispatchEvent(new Event('scrollend')));
    expect(current()).toBe('About');
  });

  // Safari and WKWebView have no `scrollend`, and a click on a section already at the line never scrolls.
  it('lets go of a clicked item after a moment when no scrollend and no spy report ever arrives', async () => {
    const io = install();
    await renderOrg('/org', { desktop: true });
    at(io.spy(), { ...ALL_BELOW, usage: 'in' });
    vi.useFakeTimers({ toFake: ['Date', 'setTimeout', 'clearTimeout'] });
    fireEvent.click(indexLink('Network'));
    await flush();
    expect(current()).toBe('Network');
    act(() => void vi.advanceTimersByTime(1000));
    expect(current()).toBe('Network');
    act(() => void vi.advanceTimersByTime(1000));
    expect(current()).toBe('Usage');
    at(io.spy(), { usage: 'above', organization: 'in' });
    expect(current()).toBe('Organization');
  });

  it('lets go of a clicked item at the reader’s own scroll', async () => {
    const io = install();
    await renderOrg('/org', { desktop: true });
    at(io.spy(), { ...ALL_BELOW, usage: 'in' });
    fireEvent.click(indexLink('Skills'));
    await flush();
    expect(current()).toBe('Skills');
    act(() => void window.dispatchEvent(new Event('wheel')));
    expect(current()).toBe('Usage');
    fireEvent.click(indexLink('Network'));
    await flush();
    expect(current()).toBe('Network');
    act(() => void window.dispatchEvent(new KeyboardEvent('keydown', { key: 'PageDown' })));
    expect(current()).toBe('Usage');
  });

  it('jumps instead of gliding when the reader prefers reduced motion', async () => {
    install();
    await renderOrg('/org', { desktop: true, reducedMotion: true });
    const scroll = vi.mocked(Element.prototype.scrollIntoView);
    fireEvent.click(indexLink('Skills'));
    await flush();
    expect(scroll).toHaveBeenLastCalledWith({ block: 'start', behavior: 'auto' });
  });

  it('keeps the route’s section at the top while the sections above it load, until the reader scrolls', async () => {
    install();
    const resized: { target: Element; fire(): void }[] = [];
    class FakeResize {
      constructor(private readonly cb: ResizeObserverCallback) {}
      observe(target: Element) {
        resized.push({ target, fire: () => this.cb([], this as unknown as ResizeObserver) });
      }
      unobserve() {}
      disconnect() {}
    }
    vi.stubGlobal('ResizeObserver', FakeResize);
    const scroll = vi.mocked(Element.prototype.scrollIntoView);
    await renderOrg('/org/network', { desktop: true });
    const sections = resized.find((r) => r.target.classList.contains('app-org-sections'))!;
    expect(sections).toBeDefined();
    const network = document.getElementById('org-network');
    expect(scroll.mock.instances.at(-1)).toBe(network);
    scroll.mockClear();

    // A list above finished loading and the page grew: back to Network, instantly.
    act(() => sections.fire());
    expect(scroll).toHaveBeenCalledTimes(1);
    expect(scroll.mock.instances[0]).toBe(network);
    expect(scroll).toHaveBeenCalledWith({ block: 'start', behavior: 'auto' });

    // The reader scrolls: from here the page is theirs.
    fireEvent.wheel(window);
    act(() => sections.fire());
    expect(scroll).toHaveBeenCalledTimes(1);
    expect(current()).toBe('Network');

    // A click on the index anchors its section anew: gliding while it travels, instantly once it has arrived.
    fireEvent.click(indexLink('Skills'));
    await flush();
    const skills = document.getElementById('org-skills');
    scroll.mockClear();
    act(() => sections.fire());
    expect(scroll.mock.instances[0]).toBe(skills);
    expect(scroll).toHaveBeenLastCalledWith({ block: 'start', behavior: 'smooth' });
    act(() => void document.dispatchEvent(new Event('scrollend')));
    act(() => sections.fire());
    expect(scroll.mock.instances[1]).toBe(skills);
    expect(scroll).toHaveBeenLastCalledWith({ block: 'start', behavior: 'auto' });
  });

  it('keeps the route’s section when there is no IntersectionObserver', async () => {
    vi.stubGlobal('IntersectionObserver', undefined);
    await renderOrg('/org/network', { desktop: true });
    expect(current()).toBe('Network');
  });

  it('leaves the phone layout alone: no index and nothing observed', async () => {
    const io = install();
    await renderOrg('/org/skills');
    expect(screen.queryByRole('navigation', { name: 'Org sections' })).toBeNull();
    expect(io.fake.instances).toHaveLength(0);
  });

  it('disconnects its observers on unmount', async () => {
    const io = install();
    const h = await renderOrg('/org', { desktop: true });
    expect(io.fake.instances.length).toBeGreaterThan(0);
    h.unmount();
    expect(io.fake.instances.every((o) => o.disconnected)).toBe(true);
  });
});

// ---- Usage ----

describe('Usage', () => {
  const eight: Usage = {
    ...goUsage,
    by_agent: [100, 90, 80, 70, 60, 50, 40, 30].map((d30, i) => ({ slug: 'a' + i, name: 'Agent ' + i, d7: d30 / 2, d30 })),
  };

  it('shows three tiles, 30 bars, the top five and everyone else', async () => {
    await renderOrg('/org/usage', { desktop: true, routes: { 'GET /api/v1/org/usage': eight } });
    const tiles = Array.from(document.querySelectorAll('.app-org-tile')).map((t) => t.textContent);
    expect(tiles).toEqual(['Today$4.2531 calls · 82% cache hit', '7 days$61.50540 calls · 79% cache hit', '30 days$210.752,210 calls · 77% cache hit']);
    expect(screen.getByText('All agents · priced at list rates')).toBeInTheDocument();

    const chart = screen.getByRole('img', { name: /Spend per day/ }) as unknown as HTMLElement;
    expect(chart.querySelectorAll('rect')).toHaveLength(30);
    for (const label of ['30 Aug', '13 Sept', 'Today']) expect(within(chart).getByText(label)).toBeInTheDocument();

    const ranked = document.querySelector('.kv-ranked') as HTMLElement;
    expect(ranked.querySelectorAll('.kv-ranked-row')).toHaveLength(6);
    expect(within(ranked).getByText('Agent 0')).toBeInTheDocument();
    expect(within(ranked).queryByText('Agent 5')).toBeNull();
    const rest = within(ranked).getByText('Everyone else · 3 agents').closest('.kv-ranked-row') as HTMLElement;
    expect(within(rest).getByText('$120')).toBeInTheDocument();
  });

  it('folds the per-agent and calls tables, closed by default', async () => {
    await renderOrg('/org/usage', { routes: { 'GET /api/v1/org/usage': eight } });
    const folds = Array.from(document.querySelectorAll('details'));
    expect(folds).toHaveLength(2);
    expect(folds.every((d) => !d.open)).toBe(true);
    const byAgent = folds[0] as HTMLElement;
    expect(within(byAgent).getByText('By agent')).toBeInTheDocument();
    expect(within(byAgent).getByText('All 8 · ranked by 30-day spend')).toBeInTheDocument();
    expect(within(byAgent).getAllByRole('row')).toHaveLength(9);
    expect(within(byAgent).getByRole('columnheader', { name: 'Share' })).toBeInTheDocument();
    expect(within(byAgent).getByText('Agent 7')).toBeInTheDocument();
    const windows = folds[1] as HTMLElement;
    expect(within(windows).getByText('Calls and tokens')).toBeInTheDocument();
    for (const w of ['Last 24 hours', 'Last 7 days', 'Last 30 days']) expect(within(windows).getByText(w)).toBeInTheDocument();
    expect(within(windows).getByRole('columnheader', { name: 'Cache hit' })).toBeInTheDocument();
    expect(within(windows).getByText('2,210')).toBeInTheDocument();
  });

  it('says in a caption how many tokens had no list price', async () => {
    await renderOrg('/org/usage');
    const note = screen.getByText(unpricedNote(goUsage.windows.d30.unpriced));
    expect(note.textContent).toBe('27,324,359 tokens this month ran on a model with no list price, so spend leaves them out.');
    expect(note.closest('details')).toBeNull();
  });

  it('says nothing about unpriced tokens when there are none', async () => {
    const clean: Usage = { ...goUsage, windows: { h24: goUsage.windows.h24, d7: { ...goUsage.windows.d7, unpriced: 0 }, d30: { ...goUsage.windows.d30, unpriced: 0 } } };
    await renderOrg('/org/usage', { routes: { 'GET /api/v1/org/usage': clean } });
    expect(screen.queryByText(/no list price/)).toBeNull();
  });

  it('writes token counts short', () => {
    expect(compact(412_000)).toBe('412k');
    expect(compact(3_100_000)).toBe('3.1M');
    expect(compact(2_000_000)).toBe('2M');
    expect(compact(999)).toBe('999');
  });

  it('draws a narrower chart on phone and no tile captions', async () => {
    await renderOrg('/org/usage');
    expect(screen.getByRole('img', { name: /Spend per day/ }).getAttribute('viewBox')).toBe('0 0 324 190');
    expect(screen.queryByText(/cache hit ·|· \d+% cache hit/)).toBeNull();
  });

  it('says what happened when the numbers do not load', async () => {
    await renderOrg('/org/usage', { routes: { 'GET /api/v1/org/usage': fail(500, 'Usage could not be read.', 'Whoever runs this Kivali server can look into it.') } });
    expect(screen.getByText('Usage could not be read.')).toBeInTheDocument();
    expect(screen.getByText('Whoever runs this Kivali server can look into it.')).toBeInTheDocument();
  });
});

// ---- Organization ----

describe('Organization', () => {
  it('saves the name once it has changed, then refreshes who you are so the sidebar follows', async () => {
    await renderOrg('/org/organization', {
      routes: { 'PUT /api/v1/org': (b: unknown) => ({ ...goOrg, name: (b as { name: string }).name }) },
    });
    const field = screen.getByLabelText('Name');
    const save = screen.getByRole('button', { name: 'Save' });
    expect(save).toBeDisabled();
    fireEvent.change(field, { target: { value: 'Plainsong Labs' } });
    expect(save).toBeEnabled();
    expect(refreshMe).not.toHaveBeenCalled();
    await click(save);
    expect(save).toBeDisabled();
    expect(lastBody('PUT', '/api/v1/org')).toEqual({ name: 'Plainsong Labs' });
    expect(field).toHaveValue('Plainsong Labs');
    expect(refreshMe).toHaveBeenCalledTimes(1);
  });

  it('saves what your agents call you, trimmed, beside the name', async () => {
    const saved: OrgData = { ...goOrg, owner_name: 'Mom' };
    await renderOrg('/org/organization', { routes: { 'PUT /api/v1/org': saved } });
    const field = screen.getByLabelText('What your agents call you');
    expect(field).toHaveValue(goOrg.owner_name);
    expect(screen.getByText(/For example Jane, Mom or CEO/)).toBeInTheDocument();
    fireEvent.change(field, { target: { value: '  Mom ' } });
    await click(screen.getByRole('button', { name: 'Save' }));
    expect(lastBody('PUT', '/api/v1/org')).toEqual({ name: goOrg.name, owner_name: 'Mom' });
    expect(field).toHaveValue('Mom');
    expect(refreshMe).toHaveBeenCalledTimes(1);
  });

  it('saves what your agents call you on a team with no name', async () => {
    const unnamed: OrgData = { ...goOrg, name: '', owner_name: '' };
    await renderOrg('/org/organization', { routes: { 'GET /api/v1/org': unnamed, 'PUT /api/v1/org': { ...unnamed, owner_name: 'Mom' } } });
    fireEvent.change(screen.getByLabelText('What your agents call you'), { target: { value: 'Mom' } });
    const save = screen.getByRole('button', { name: 'Save' });
    expect(save).toBeEnabled();
    await click(save);
    expect(lastBody('PUT', '/api/v1/org')).toEqual({ name: '', owner_name: 'Mom' });
  });

  it('does not clear a team name it has', async () => {
    await renderOrg('/org/organization');
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: '  ' } });
    fireEvent.change(screen.getByLabelText('What your agents call you'), { target: { value: 'Mom' } });
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
  });

  it('does not refresh when the save is refused', async () => {
    await renderOrg('/org/organization', { routes: { 'PUT /api/v1/org': fail(400, 'That name is too long.', 'You can shorten it.') } });
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'x' } });
    await click(screen.getByRole('button', { name: 'Save' }));
    expect(screen.getByText('That name is too long.')).toBeInTheDocument();
    expect(refreshMe).not.toHaveBeenCalled();
  });

  const noLogo: OrgData = { name: 'Plainsong', has_logo: false, kind: '', owner_name: '' };
  const png = () => new File(['png'], 'logo.png', { type: 'image/png' });

  it('leads with the logo when one is set: Replace and Remove, the format caption, no drop zone', async () => {
    await renderOrg('/org/organization');
    expect(screen.getByRole('img', { name: 'Plainsong logo' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Replace' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Remove' })).toBeInTheDocument();
    expect(screen.getByText(/PNG, square, at least 64 px/)).toBeInTheDocument();
    expect(screen.queryByText('Drop a square logo')).toBeNull();
    expect(document.querySelector('.kv-drop')).toBeNull();
  });

  it('Replace opens the picker and posts the chosen file as multipart', async () => {
    vi.mocked(pickFiles).mockResolvedValue([png()]);
    await renderOrg('/org/organization', { routes: { 'POST /api/v1/org/logo': goOrg } });
    await click(screen.getByRole('button', { name: 'Replace' }));
    expect(pickFiles).toHaveBeenCalledWith({ multiple: false, accept: 'image/png' });
    expect((lastBody('POST', '/api/v1/org/logo') as FormData).get('logo')).toBeInstanceOf(File);
    expect(refreshMe).toHaveBeenCalledTimes(1);
  });

  it('a cancelled picker uploads nothing', async () => {
    vi.mocked(pickFiles).mockResolvedValue([]);
    await renderOrg('/org/organization');
    await click(screen.getByRole('button', { name: 'Replace' }));
    expect(called('POST', '/api/v1/org/logo')).toBe(false);
  });

  it('dropping an image on the logo tile replaces the logo, and the tile lights while it hovers', async () => {
    await renderOrg('/org/organization', { routes: { 'POST /api/v1/org/logo': goOrg } });
    const tile = screen.getByTestId('logo-tile');
    fireEvent.dragOver(tile);
    expect(tile).toHaveClass('is-over');
    fireEvent.dragLeave(tile);
    expect(tile).not.toHaveClass('is-over');
    fireEvent.drop(tile, { dataTransfer: { files: [png()] } });
    await flush();
    expect((lastBody('POST', '/api/v1/org/logo') as FormData).get('logo')).toBeInstanceOf(File);
  });

  it('Remove deletes the logo and returns to the drop zone, refreshing who you are', async () => {
    await renderOrg('/org/organization', { routes: { 'DELETE /api/v1/org/logo': noLogo } });
    await click(screen.getByRole('button', { name: 'Remove' }));
    expect(called('DELETE', '/api/v1/org/logo')).toBe(true);
    expect(refreshMe).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('button', { name: 'Remove' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Replace' })).toBeNull();
    expect(screen.getByText('Drop a square logo')).toBeInTheDocument();
  });

  it('without a logo shows the monogram and the drop zone; an upload switches to the logo state', async () => {
    await renderOrg('/org/organization', { routes: { 'GET /api/v1/org': noLogo, 'POST /api/v1/org/logo': goOrg } });
    expect(screen.getByText('Drop a square logo')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Replace' })).toBeNull();
    await drop(png());
    expect((lastBody('POST', '/api/v1/org/logo') as FormData).get('logo')).toBeInstanceOf(File);
    expect(refreshMe).toHaveBeenCalledTimes(1);
    expect(screen.queryByText('Drop a square logo')).toBeNull();
    expect(screen.getByRole('img', { name: 'Plainsong logo' })).toBeInTheDocument();
  });

  it('shows the server sentence when a logo is refused, and keeps the current logo', async () => {
    vi.mocked(pickFiles).mockResolvedValue([png()]);
    await renderOrg('/org/organization', { routes: { 'POST /api/v1/org/logo': fail(400, 'The logo must be square.', 'You can upload a square PNG.') } });
    await click(screen.getByRole('button', { name: 'Replace' }));
    expect(screen.getByText('The logo must be square.')).toBeInTheDocument();
    expect(refreshMe).not.toHaveBeenCalled();
    expect(screen.getByRole('img', { name: 'Plainsong logo' })).toBeInTheDocument();
  });
});

// ---- Handbook ----

describe('Handbook', () => {
  it('shows the opening, then sections folded with line counts, and Edit at the foot', async () => {
    await renderOrg('/org/handbook');
    expect(screen.getByText('We ship.')).toBeInTheDocument();
    const money = screen.getByRole('button', { name: /Money/ });
    expect(money).toHaveAttribute('aria-expanded', 'false');
    expect(money).toHaveTextContent('1 line');
    expect(screen.queryByText('Under budget')).toBeNull();
    fireEvent.click(money);
    expect(screen.getByText('Under budget')).toBeInTheDocument();
    expect(money).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByText('Most changes arrive from your Chief of Staff as proposals in Needs you.')).toBeInTheDocument();
    expect(screen.queryByText(/starter text/)).toBeNull();
  });

  it('says on desktop that it applies to every agent and how many sections it has', async () => {
    await renderOrg('/org/handbook', { desktop: true });
    expect(screen.getByText(/^Applies to every agent · 1 section · updated /)).toBeInTheDocument();
  });

  it('says when nobody has saved it yet', async () => {
    await renderOrg('/org/handbook', { routes: { 'GET /api/v1/org/handbook': { ...goHandbook, draft: true } } });
    expect(screen.getByText(/starter text/)).toBeInTheDocument();
  });

  it('edits, saves and shows the saved text', async () => {
    const saved: Handbook = { ...goHandbook, content: 'We ship carefully.\n## Money\n- Under budget\n', sections: [{ title: 'Opening', lines: ['We ship carefully.'], line_count: 1 }, goHandbook.sections[1]!] };
    await renderOrg('/org/handbook', { routes: { 'PUT /api/v1/org/handbook': saved } });
    fireEvent.click(screen.getByRole('button', { name: 'Edit' }));
    const box = screen.getByLabelText('Handbook');
    expect(box).toHaveValue(goHandbook.content);
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
    fireEvent.change(box, { target: { value: saved.content } });
    await click(screen.getByRole('button', { name: 'Save' }));
    expect(screen.getByText('We ship carefully.')).toBeInTheDocument();
    expect(lastBody('PUT', '/api/v1/org/handbook')).toEqual({ content: saved.content });
    expect(screen.getByRole('button', { name: 'Edit' })).toBeInTheDocument();
  });

  it('cancels an edit without saving', async () => {
    await renderOrg('/org/handbook');
    fireEvent.click(screen.getByRole('button', { name: 'Edit' }));
    fireEvent.change(screen.getByLabelText('Handbook'), { target: { value: 'x' } });
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.getByText('We ship.')).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'PUT')).toBe(false);
  });
});

// ---- Project files ----

describe('Project files', () => {
  const first = goFiles.files[0]!;
  const second = { ...first, sha: 'b'.repeat(64), name: 'brief.md', extracted: false };
  const two: ProjectFiles = { files: [first, second] };

  it('lists files with size, date and extraction state, and a count and total on desktop', async () => {
    await renderOrg('/org/files', { desktop: true });
    expect(screen.getByText('plan.pdf')).toBeInTheDocument();
    expect(screen.getByText(/84 KB · uploaded/)).toBeInTheDocument();
    expect(screen.getByText('Text extracted')).toBeInTheDocument();
    expect(screen.getByText('1 file · 84 KB')).toBeInTheDocument();
  });

  it('downloads a file with a button, not a link', async () => {
    const created = vi.fn(() => 'blob:plan');
    vi.stubGlobal('URL', Object.assign(URL, { createObjectURL: created, revokeObjectURL: vi.fn() }));
    const anchorClick = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    await renderOrg('/org/files', { routes: { ['GET ' + first.url]: new Blob(['pdf']) } });
    expect(screen.queryByRole('link', { name: 'plan.pdf' })).toBeNull();
    await click(screen.getByRole('button', { name: 'Download plan.pdf' }));
    expect(called('GET', first.url)).toBe(true);
    expect(created).toHaveBeenCalled();
    expect(anchorClick).toHaveBeenCalledTimes(1);
    expect((anchorClick.mock.instances[0] as unknown as HTMLAnchorElement).download).toBe('plan.pdf');
  });

  it('uploads and refreshes the list from the response', async () => {
    await renderOrg('/org/files', { routes: { 'POST /api/v1/org/files': two } });
    await drop(new File(['hi'], 'brief.md'));
    expect(screen.getByText('brief.md')).toBeInTheDocument();
    expect(screen.getByText('Stored only')).toBeInTheDocument();
    expect((lastBody('POST', '/api/v1/org/files') as FormData).getAll('files[]')).toHaveLength(1);
  });

  it('removes one file', async () => {
    await renderOrg('/org/files', { routes: { ['DELETE /api/v1/org/files/' + first.sha]: { files: [] } } });
    await click(screen.getByRole('button', { name: 'Remove plan.pdf' }));
    expect(screen.getByText(/No project files yet/)).toBeInTheDocument();
    expect(called('DELETE', '/api/v1/org/files/' + first.sha)).toBe(true);
  });

  it('removes the ticked files in one call', async () => {
    await renderOrg('/org/files', { routes: { 'GET /api/v1/org/files': two, 'POST /api/v1/org/files/bulk-delete': { files: [second] } } });
    expect(screen.queryByRole('button', { name: /selected/ })).toBeNull();
    fireEvent.click(screen.getByRole('checkbox', { name: 'Select plan.pdf' }));
    await click(screen.getByRole('button', { name: 'Remove 1 selected' }));
    expect(screen.queryByText('plan.pdf')).toBeNull();
    expect(lastBody('POST', '/api/v1/org/files/bulk-delete')).toEqual({ shas: [first.sha] });
    expect(screen.queryByRole('button', { name: /selected/ })).toBeNull();
  });
});

// ---- Skills ----

describe('Skills', () => {
  const enabled: Skills = { skills: [goSkills.skills[0]!, { ...goSkills.skills[1]!, enabled: true }] };

  it('shows version and description, the Built in badge and a count; only custom skills can be removed', async () => {
    await renderOrg('/org/skills', { desktop: true });
    expect(screen.getByText('v1.2.0 · Read and fill PDFs.')).toBeInTheDocument();
    expect(screen.getByText('Built in')).toBeInTheDocument();
    expect(screen.getByText('2 installed · 1 off')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Remove pdf' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Remove quote-check' })).toBeInTheDocument();
  });

  it('gives every switch its own name', async () => {
    await renderOrg('/org/skills');
    const names = screen.getAllByRole('switch').map((sw) => sw.getAttribute('id') && document.querySelector(`label[for="${sw.id}"]`)?.textContent);
    expect(names).toEqual(['Enable pdf', 'Enable quote-check']);
  });

  it('turns a skill on with its switch', async () => {
    await renderOrg('/org/skills', { routes: { 'POST /api/v1/org/skills/quote-check/enable': enabled } });
    const sw = screen.getByRole('switch', { name: 'Enable quote-check' });
    expect(sw).toHaveAttribute('aria-checked', 'false');
    await click(sw);
    expect(sw).toHaveAttribute('aria-checked', 'true');
    expect(called('POST', '/api/v1/org/skills/quote-check/enable')).toBe(true);
  });

  it('turns a skill off', async () => {
    await renderOrg('/org/skills', { routes: { 'POST /api/v1/org/skills/pdf/disable': { skills: [{ ...goSkills.skills[0]!, enabled: false }, goSkills.skills[1]!] } } });
    const sw = screen.getByRole('switch', { name: 'Enable pdf' });
    await click(sw);
    expect(sw).toHaveAttribute('aria-checked', 'false');
    expect(called('POST', '/api/v1/org/skills/pdf/disable')).toBe(true);
  });

  it('uploads from the Upload a skill button', async () => {
    vi.mocked(pickFiles).mockResolvedValue([new File(['zip'], 'quote-check.zip')]);
    await renderOrg('/org/skills', { routes: { 'POST /api/v1/org/skills': enabled } });
    await click(screen.getByRole('button', { name: 'Upload a skill' }));
    expect(pickFiles).toHaveBeenCalledWith({ multiple: false });
    expect((lastBody('POST', '/api/v1/org/skills') as FormData).get('file')).toBeInstanceOf(File);
    expect(screen.getByRole('switch', { name: 'Enable quote-check' })).toHaveAttribute('aria-checked', 'true');
  });

  it('reads a downgrade from the 409 body the client keeps on ApiError', async () => {
    const actual = await vi.importActual<typeof import('../../api/client')>('../../api/client');
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(JSON.stringify(goDowngrade), { status: 409 }))));
    const err = await actual.apiPostForm('/api/v1/org/skills', new FormData()).catch((e: unknown) => e);
    vi.unstubAllGlobals();
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).body).toEqual(goDowngrade);
    expect(downgradeOf(err)).toEqual(goDowngrade);
    expect(downgradeOf(fail(409, 'Taken.', 'You.'))).toBeNull();
    expect(downgradeOf(fail(400, goDowngrade.error, goDowngrade.who, goDowngrade))).toBeNull();
  });

  it('asks before replacing a skill with an older version, then re-sends with confirm_downgrade', async () => {
    let posts = 0;
    vi.mocked(pickFiles).mockResolvedValue([new File(['zip'], 'quote-check.zip')]);
    await renderOrg('/org/skills', {
      routes: { 'POST /api/v1/org/skills': () => (posts++ === 0 ? fail(409, goDowngrade.error, goDowngrade.who, goDowngrade) : enabled) },
    });
    await click(screen.getByRole('button', { name: 'Upload a skill' }));
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByText('Replace version 0.3.1 with older version 0.3.0?')).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toBeInTheDocument();
    expect((lastBody('POST', '/api/v1/org/skills') as FormData).get('confirm_downgrade')).toBeNull();

    await click(within(dialog).getByRole('button', { name: 'Replace' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect((lastBody('POST', '/api/v1/org/skills') as FormData).get('confirm_downgrade')).toBe('true');
    expect(screen.getByRole('switch', { name: 'Enable quote-check' })).toHaveAttribute('aria-checked', 'true');
  });

  it('cancelling the downgrade sends nothing more', async () => {
    vi.mocked(pickFiles).mockResolvedValue([new File(['zip'], 'quote-check.zip')]);
    await renderOrg('/org/skills', { routes: { 'POST /api/v1/org/skills': fail(409, goDowngrade.error, goDowngrade.who, goDowngrade) } });
    await click(screen.getByRole('button', { name: 'Upload a skill' }));
    await click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(calls.filter((c) => c.method === 'POST')).toHaveLength(1);
  });

  it('shows the refusal for any other upload failure', async () => {
    vi.mocked(pickFiles).mockResolvedValue([new File(['x'], 'nope.zip')]);
    await renderOrg('/org/skills', { routes: { 'POST /api/v1/org/skills': fail(400, 'That file is not a skill.', 'Whoever made the file can fix it.') } });
    await click(screen.getByRole('button', { name: 'Upload a skill' }));
    expect(screen.getByText('That file is not a skill.')).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('confirms before removing a skill, with Cancel secondary and a named danger button', async () => {
    await renderOrg('/org/skills', { routes: { 'DELETE /api/v1/org/skills/quote-check': { skills: [goSkills.skills[0]!] } } });
    await click(screen.getByRole('button', { name: 'Remove quote-check' }));
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByText('Remove quote-check?')).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveClass('kv-btn--secondary');
    expect(called('DELETE', '/api/v1/org/skills/quote-check')).toBe(false);
    await click(within(dialog).getByRole('button', { name: 'Remove' }));
    expect(screen.queryByRole('switch', { name: 'Enable quote-check' })).toBeNull();
    expect(called('DELETE', '/api/v1/org/skills/quote-check')).toBe(true);
  });
});


// ---- Network ----

describe('Network', () => {
  it('allows and removes a host', async () => {
    const added: Network = { hosts: [...goNetwork.hosts, { host: 'api.github.com' }] };
    await renderOrg('/org/network', {
      desktop: true,
      routes: { 'POST /api/v1/org/network': added, 'DELETE /api/v1/org/network/api.anthropic.com': { hosts: [goNetwork.hosts[0]!, added.hosts[2]!] } },
    });
    expect(screen.getByText('*.pypi.org')).toBeInTheDocument();
    expect(screen.getByText('2 hosts agents can reach · changes apply at once')).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText('Allow a host'), { target: { value: 'api.github.com' } });
    await click(screen.getByRole('button', { name: 'Allow' }));
    expect(screen.getByText('api.github.com')).toBeInTheDocument();
    expect(lastBody('POST', '/api/v1/org/network')).toEqual({ host: 'api.github.com' });
    await click(screen.getByRole('button', { name: 'Remove api.anthropic.com' }));
    expect(screen.queryByText('api.anthropic.com')).toBeNull();
  });
});

// ---- Backup and restore ----

describe('Backup and restore', () => {
  it('offers the download and says restore lives in setup', async () => {
    await renderOrg('/org/backup');
    expect(screen.getByRole('button', { name: 'Download a backup' })).toBeInTheDocument();
    expect(screen.getByText(/Restore is offered only on a fresh install, from the first setup screen\./)).toBeInTheDocument();
    expect(document.querySelector('input[type="file"]')).toBeNull();
  });

  function backupForm(): HTMLFormElement {
    const f = document.querySelector('form[action="/api/v1/org/backup"]');
    if (!(f instanceof HTMLFormElement)) throw new Error('no backup form');
    return f;
  }

  function backupFrame(): HTMLIFrameElement {
    const f = document.querySelector(`iframe[name="${backupForm().target}"]`);
    if (!(f instanceof HTMLIFrameElement)) throw new Error('no backup frame');
    return f;
  }

  it('hands the archive to the browser’s downloads as a real form POST, never through the page', async () => {
    const submit = vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(() => {});
    const created = vi.fn(() => 'blob:backup');
    vi.stubGlobal('URL', Object.assign(URL, { createObjectURL: created }));
    await renderOrg('/org/backup');
    const form = backupForm();
    expect(form.getAttribute('method')).toBe('post');
    expect(form.target).not.toBe('');
    expect(form.target).not.toBe('_blank');
    expect(backupFrame().hidden).toBe(true);
    vi.useFakeTimers({ toFake: ['Date', 'setTimeout', 'clearTimeout'] });
    const button = screen.getByRole('button', { name: 'Download a backup' });
    await click(button);
    expect(submit).toHaveBeenCalledTimes(1);
    expect(submit.mock.instances[0]).toBe(form);
    expect(calls.some((c) => c.url === '/api/v1/org/backup')).toBe(false);
    expect(created).not.toHaveBeenCalled();
    expect(screen.getByRole('status')).toHaveTextContent(/browser’s downloads/);
    expect(screen.getByRole('status')).toHaveTextContent(/close this tab/);
    expect(button).toBeDisabled();
    act(() => void vi.advanceTimersByTime(BACKUP_DEBOUNCE_MS));
    expect(button).not.toBeDisabled();
  });

  it('ignores the frame’s first blank load', async () => {
    await renderOrg('/org/backup');
    fireEvent.load(backupFrame());
    await flush();
    expect(screen.queryByText('The backup did not start.')).toBeNull();
  });

  it('says what happened when the server answers with a refusal instead of the archive', async () => {
    vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(() => {});
    await renderOrg('/org/backup');
    await click(screen.getByRole('button', { name: 'Download a backup' }));
    const frame = backupFrame();
    frame.contentDocument!.body.textContent = JSON.stringify({ error: 'unauthenticated', who: 'sign in again' });
    fireEvent.load(frame);
    await flush();
    expect(screen.getByText('unauthenticated')).toBeInTheDocument();
    expect(screen.getByText('sign in again')).toBeInTheDocument();
    expect(screen.queryByRole('status')).toBeNull();
  });

  it('says the backup did not start when the frame shows a page that is not a refusal', async () => {
    vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(() => {});
    await renderOrg('/org/backup');
    await click(screen.getByRole('button', { name: 'Download a backup' }));
    const frame = backupFrame();
    frame.contentDocument!.body.textContent = 'This site can’t be reached';
    fireEvent.load(frame);
    await flush();
    expect(screen.getByText('The backup did not start.')).toBeInTheDocument();
  });
});

// ---- About ----

describe('About', () => {
  it('names the version and how the model calls are paid for', async () => {
    await renderOrg('/org/about');
    expect(screen.getByText('Kivali 0.15.0 · Claude Max')).toBeInTheDocument();
  });

  it('keeps the version when setup cannot be read', async () => {
    await renderOrg('/org/about', { routes: { 'GET /api/v1/setup': fail(500, 'no', 'nobody') } });
    expect(screen.getByText('Kivali 0.15.0')).toBeInTheDocument();
  });

  it('writes the line from the version and the credential', () => {
    expect(versionLabel('v0.42.1')).toBe('Kivali 0.42.1');
    expect(versionLabel('dev')).toBe('Kivali dev');
    expect(versionLabel(undefined)).toBe('Kivali');
    const credential = { ready: true, provider: 'claude', present: true, who: '', billing: 'Amazon Bedrock', guidance: '' };
    expect(aboutLine('0.42.1', credential)).toBe('Kivali 0.42.1 · Amazon Bedrock');
    expect(aboutLine('0.42.1', { ...credential, billing: '' })).toBe('Kivali 0.42.1 · Signed in to Claude');
    expect(aboutLine('0.42.1', { ...credential, present: false, billing: '' })).toBe('Kivali 0.42.1 · Not signed in to Claude');
    expect(aboutLine('0.42.1', undefined)).toBe('Kivali 0.42.1');
  });
});
