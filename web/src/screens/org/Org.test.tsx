import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, apiDelete, apiGet, apiGetBlob, apiPost, apiPostForm, apiPut } from '../../api/client';
import type { Handbook, IntegrationPage as IntegrationPageData, Me, Network, OrgSoftware, Org as OrgData, ProjectFiles, SetupFlow, Skills, Usage } from '../../api/types.gen';
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
import { InstallScreen, bundleLine } from './InstallScreen';
import { IntegrationPage, deleteConnectionWords, setupReturnOf, signInLine, signsInAt } from './IntegrationPage';
import { SETUP_POLL_MS, codeLifeWords, progressWords } from './SetupFlowPanel';
import { whenLabel } from './integrationParts';
import { Org, headerMeta } from './OrgPage';
import {
  goAccessChoices,
  goAccessWrite,
  goConnectionCreate,
  goConnectionWrite,
  goDowngrade,
  goFiles,
  goHandbook,
  goIntegrationAudit,
  goIntegrationLogs,
  goIntegrationPage,
  goIntegrationPageMultiple,
  goIntegrationPageSignIn,
  goIntegrationPageSignInAgain,
  goIntegrationPageSignInElsewhere,
  goSetupFlowDone,
  goSetupFlowFailed,
  goSetupFlowForm,
  goSetupFlowGo,
  goSetupFlowGoNoReturn,
  goSetupFlowRunning,
  goSetupFlowWait,
  goIntegrationUploadRefused,
  goIntegrationVersion,
  goIntegrationVersionBuiltin,
  goIntegrationVersionShipped,
  goIntegrationVersionSignIn,
  goIntegrationVersionSetup,
  goIntegrationVersionRegistered,
  goIntegrationPageSetup,
  goIntegrations,
  goNetwork,
  goOrg,
  goOrgSoftware,
  goSkills,
  goSoftwareAdd,
  goUsage,
} from './orgFixtures';
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
    'GET /api/v1/org/software': goOrgSoftware,
    'GET /api/v1/org/integrations': goIntegrations,
    'GET /api/v1/org/integrations/hubspot': goIntegrationPage,
    'GET /api/v1/org/integrations/hubspot/audit': goIntegrationAudit,
    'GET /api/v1/org/integrations/hubspot/logs': goIntegrationLogs,
    'GET /api/v1/org/integrations/hubspot/versions/1.3.0': goIntegrationVersion,
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
      { path: '/org/integrations/:name', element: <IntegrationPage /> },
      { path: '/org/integrations/:name/versions/:version', element: <InstallScreen /> },
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
    for (const name of ['Organization', 'Handbook', 'Project files', 'Integrations', 'Network', 'Software', 'Backup and restore', 'About']) {
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
    const names = ['Usage', 'Organization', 'Handbook', 'Project files', 'Skills', 'Integrations', 'Network', 'Software', 'Backup and restore', 'About'];
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
      /** The strip observer on the ten sections. */
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
    integrations: 'below',
    network: 'below',
    software: 'below',
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
    expect(io.spy().targets.map((el) => el.id)).toEqual(['usage', 'organization', 'handbook', 'files', 'skills', 'integrations', 'network', 'software', 'backup', 'about'].map((k) => 'org-' + k));

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

  // Loading a section's URL on a page too short to bring it up to the line (a Network list of two hosts) ends with
  // the end marker on screen; the index must still show the section asked for, not About.
  it('keeps the section loaded by URL current at the bottom of the page until the reader scrolls', async () => {
    const io = install();
    await renderOrg('/org/network', { desktop: true });
    at(io.spy(), { ...ALL_BELOW, usage: 'above', organization: 'above', handbook: 'above', files: 'above', skills: 'above', integrations: 'above' });
    act(() => io.tail().fire([{ target: 'org-end', isIntersecting: true, top: 700 }]));
    expect(current()).toBe('Network');
    act(() => void window.dispatchEvent(new Event('wheel')));
    at(io.spy(), { ...ALL_BELOW, usage: 'above', organization: 'above', handbook: 'above', files: 'above', skills: 'above', integrations: 'above' });
    act(() => io.tail().fire([{ target: 'org-end', isIntersecting: true, top: 700 }]));
    expect(current()).toBe('About');
  });

  it('gives About the bottom of the page even when its top never reaches the line', async () => {
    const io = install();
    await renderOrg('/org', { desktop: true });
    at(io.spy(), { ...ALL_BELOW, usage: 'above', organization: 'above', handbook: 'above', files: 'above', skills: 'above', integrations: 'above', network: 'above', backup: 'in' });
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
    at(io.spy(), { skills: 'above', integrations: 'above', network: 'in' });
    expect(current()).toBe('Network');
    at(io.spy(), { network: 'below', integrations: 'below', skills: 'in' });
    expect(current()).toBe('Skills');
    expect(h.router.state.location.pathname).toBe('/org/network');
  });

  // The clicked section stays current when the page cannot bring it up to the line: the reader asked for it by
  // name, and the bottom of the page goes to About only once they scroll on themselves.
  it('keeps the clicked section when the scroll ends short of it, until the reader scrolls on', async () => {
    const io = install();
    await renderOrg('/org', { desktop: true });
    at(io.spy(), { ...ALL_BELOW, usage: 'above', organization: 'above', handbook: 'above', files: 'above', skills: 'above', integrations: 'above', network: 'in' });
    expect(current()).toBe('Network');
    fireEvent.click(indexLink('Backup and restore'));
    await flush();
    expect(current()).toBe('Backup and restore');
    act(() => io.tail().fire([{ target: 'org-end', isIntersecting: true, top: 700 }]));
    expect(current()).toBe('Backup and restore');
    act(() => void document.dispatchEvent(new Event('scrollend')));
    expect(current()).toBe('Backup and restore');
    act(() => void window.dispatchEvent(new Event('wheel')));
    at(io.spy(), { ...ALL_BELOW, usage: 'above', organization: 'above', handbook: 'above', files: 'above', skills: 'above', integrations: 'above', network: 'in' });
    act(() => io.tail().fire([{ target: 'org-end', isIntersecting: true, top: 700 }]));
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
    // The wide layout draws the Integrations section too, whose list has a built-in of its own.
    expect(within(screen.getByRole('region', { name: 'Skills' })).getByText('Built in')).toBeInTheDocument();
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
  // The permanent host is listed and marked, with no Remove: the server
  // refuses that removal, so a button for it would only fail.
  it('allows and removes a host, and shows the permanent one without Remove', async () => {
    const added: Network = { ...goNetwork, hosts: [...goNetwork.hosts, { host: 'api.github.com', permanent: false }] };
    await renderOrg('/org/network', {
      desktop: true,
      routes: { 'POST /api/v1/org/network': added, 'DELETE /api/v1/org/network/*.pypi.org': { ...goNetwork, hosts: [goNetwork.hosts[1]!, added.hosts[2]!] } },
    });
    expect(screen.getByText('*.pypi.org')).toBeInTheDocument();
    expect(screen.getByText('deb.debian.org')).toBeInTheDocument();
    expect(screen.getByText('Permanent')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Remove deb.debian.org' })).toBeNull();
    expect(screen.getByText('2 hosts agent shells can reach · an entry covers every name under it · changes apply at once')).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText('Allow a host'), { target: { value: 'api.github.com' } });
    await click(screen.getByRole('button', { name: 'Allow' }));
    expect(screen.getByText('api.github.com')).toBeInTheDocument();
    expect(lastBody('POST', '/api/v1/org/network')).toEqual({ host: 'api.github.com' });
    await click(screen.getByRole('button', { name: 'Remove *.pypi.org' }));
    expect(screen.queryByText('*.pypi.org')).toBeNull();
    expect(screen.getByText('deb.debian.org')).toBeInTheDocument();
  });

  // Guards the team-wide view of agents with no network limits (network.json's exempt list): each must be
  // named, comma-separated, linking to the Security view of its About tab, where the switch is. A missing
  // line, a name that is not a link, or a link that lands anywhere else fails this.
  it('names the agents with no network limits, each linking to its Security view', async () => {
    const exempt: Network = { ...goNetwork, exempt: [...goNetwork.exempt, { slug: 'engineering-lead', name: 'Engineering lead' }] };
    await renderOrg('/org/network', { desktop: true, routes: { 'GET /api/v1/org/network': exempt } });
    const line = screen.getByText(/^No network limits:/);
    expect(line).toHaveTextContent('No network limits: Dana, Engineering lead');
    expect(within(line).getByRole('link', { name: 'Dana' })).toHaveAttribute('href', '/agents/dana/about?view=security');
    expect(within(line).getByRole('link', { name: 'Engineering lead' })).toHaveAttribute('href', '/agents/engineering-lead/about?view=security');
  });

  // Guards the quiet default: with every agent limited, the section says nothing about limits. A stray
  // "No network limits:" with no names after it fails this.
  it('says nothing about limits when every agent has them', async () => {
    await renderOrg('/org/network', { desktop: true, routes: { 'GET /api/v1/org/network': { ...goNetwork, exempt: [] } } });
    expect(screen.getByText('deb.debian.org')).toBeInTheDocument();
    expect(screen.queryByText(/No network limits/)).toBeNull();
  });
});

// ---- Software ----

describe('Software', () => {
  it('lists the packages, adds several at once, and removes one', async () => {
    const added: OrgSoftware = { packages: ['bad name', 'ffmpeg', 'git', 'jq', 'poppler-utils'], invalid: ['bad name'], unchecked: [] };
    const removed: OrgSoftware = { packages: ['bad name', 'git', 'jq', 'poppler-utils'], invalid: ['bad name'], unchecked: [] };
    await renderOrg('/org/software', {
      desktop: true,
      routes: { 'POST /api/v1/org/software': added, 'DELETE /api/v1/org/software/ffmpeg': removed },
    });
    expect(screen.getByText('git')).toBeInTheDocument();
    expect(screen.getByText('3 packages for every agent')).toBeInTheDocument();
    expect(screen.getByText(/restarts the agents it affects/)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText('Add software'), { target: { value: 'ffmpeg, poppler-utils' } });
    await click(screen.getByRole('button', { name: 'Add' }));
    // Only the new names go up: the server adds them to the list as it stands.
    expect(lastBody('POST', '/api/v1/org/software')).toEqual({ packages: ['ffmpeg', 'poppler-utils'] });
    // The body has the keys the Go server reads.
    expect(Object.keys(lastBody('POST', '/api/v1/org/software') as object)).toEqual(Object.keys(goSoftwareAdd));
    expect(screen.getByText('poppler-utils')).toBeInTheDocument();
    await click(screen.getByRole('button', { name: 'Remove ffmpeg' }));
    expect(calls.filter((c) => c.method === 'DELETE' && c.url === '/api/v1/org/software/ffmpeg')).toHaveLength(1);
    expect(screen.queryByText('ffmpeg')).toBeNull();
  });

  it('says so when nothing is installed and shows the server’s refusal of a bad name', async () => {
    await renderOrg('/org/software', {
      desktop: true,
      routes: { 'GET /api/v1/org/software': { packages: [], invalid: [], unchecked: [] }, 'POST /api/v1/org/software': fail(400, '"rm -rf" is not a Debian package name', 'you') },
    });
    expect(screen.getByText(/No extra software/)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText('Add software'), { target: { value: 'rm -rf' } });
    await click(screen.getByRole('button', { name: 'Add' }));
    expect(await screen.findByText('"rm -rf" is not a Debian package name')).toBeInTheDocument();
  });

  it('marks a listed name that is not a Debian package name as not installed, and removes it', async () => {
    await renderOrg('/org/software', {
      desktop: true,
      routes: { 'DELETE /api/v1/org/software/bad%20name': { packages: ['git', 'jq'], invalid: [], unchecked: [] } },
    });
    expect(screen.getByText('bad name')).toBeInTheDocument();
    expect(screen.getAllByText('Not a Debian package name, so it is not installed')).toHaveLength(1);
    await click(screen.getByRole('button', { name: 'Remove bad name' }));
    expect(calls.filter((c) => c.method === 'DELETE' && c.url === '/api/v1/org/software/bad%20name')).toHaveLength(1);
    expect(screen.queryByText('bad name')).toBeNull();
    expect(screen.queryByText('Not a Debian package name, so it is not installed')).toBeNull();
  });

  it('says which names were added without checking them, until dismissed', async () => {
    const added: OrgSoftware = { packages: ['ffmpg', 'git', 'jq'], invalid: [], unchecked: ['ffmpg'] };
    await renderOrg('/org/software', {
      desktop: true,
      routes: { 'GET /api/v1/org/software': { packages: ['git', 'jq'], invalid: [], unchecked: [] }, 'POST /api/v1/org/software': added },
    });
    expect(screen.queryByText(/couldn’t be reached/)).toBeNull();
    fireEvent.change(screen.getByLabelText('Add software'), { target: { value: 'ffmpg' } });
    await click(screen.getByRole('button', { name: 'Add' }));
    expect(await screen.findByText('Added ffmpg, but Debian’s package list couldn’t be reached to validate it')).toBeInTheDocument();
    await click(screen.getByRole('button', { name: /dismiss/i }));
    expect(screen.queryByText(/couldn’t be reached/)).toBeNull();
    expect(screen.getByText('ffmpg')).toBeInTheDocument();
  });
});

// ---- Integrations ----

describe('Integrations', () => {
  // Guards the list's reading of integrations.json: each row's version, status badge and agent count, the row
  // opening the integration's page, the version waiting for approval linking to its install screen, and the
  // head's counts. A field the list drops, a badge in the wrong tone or a link to the wrong place fails here.
  it('lists each integration with its version, status and agents, and links a waiting version to its install screen', async () => {
    await renderOrg('/org/integrations', { desktop: true });
    const section = within(screen.getByRole('region', { name: 'Integrations' }));
    expect(section.getByText('3 installed · 1 waiting')).toBeInTheDocument();
    const hubspot = section.getByRole('link', { name: /HubSpot/ });
    expect(hubspot).toHaveAttribute('href', '/org/integrations/hubspot');
    expect(within(hubspot).getByText('v1.2.0')).toBeInTheDocument();
    expect(within(hubspot).getByText('Connected')).toHaveClass('kv-badge--success');
    expect(within(hubspot).getByText('2 agents')).toBeInTheDocument();
    const weather = section.getByRole('link', { name: /Weather/ });
    expect(weather).toHaveAttribute('href', '/org/integrations/weather');
    expect(within(weather).getByText('Pending')).toHaveClass('kv-badge--signal');
    expect(within(weather).getByText('No agents')).toBeInTheDocument();
    expect(section.getByRole('link', { name: 'Version 1.3.0 waits for your approval' })).toHaveAttribute('href', '/org/integrations/hubspot/versions/1.3.0');
    expect(section.getAllByRole('link', { name: /waits for your approval/ })).toHaveLength(1);
    expect(within(hubspot).queryByText('Built in')).toBeNull();
    // A built-in record this version of Kivali does not carry says so, rather than "Not running".
    const maps = section.getByRole('link', { name: /Maps/ });
    expect(within(maps).getByText('Not in this version of Kivali')).toHaveClass('kv-badge--neutral');
    expect(within(maps).getByText('Built in')).toBeInTheDocument();
  });

  // Guards the quiet states: a built-in integration carries the Built in badge beside its status, and an empty
  // list says so instead of drawing an empty container.
  it('marks a built-in integration, and says when none is installed', async () => {
    const builtin = { integrations: [{ ...goIntegrations.integrations[1]!, builtin: true }] };
    const h = await renderOrg('/org/integrations', { routes: { 'GET /api/v1/org/integrations': builtin } });
    expect(within(screen.getByRole('link', { name: /Weather/ })).getByText('Built in')).toHaveClass('kv-badge--outline');
    h.unmount();
    await renderOrg('/org/integrations', { routes: { 'GET /api/v1/org/integrations': { integrations: [] } } });
    expect(screen.getByText('No integrations installed.')).toBeInTheDocument();
  });

  // Guards the upload: one .zip goes up as the multipart field `file`, and the version the server stored leads
  // straight to its install screen, where the review happens. Another field name, or landing anywhere but that
  // version's screen, fails here.
  it('uploads a bundle and opens the install screen of the version it stored', async () => {
    vi.mocked(pickFiles).mockResolvedValue([new File(['zip'], 'hubspot.zip')]);
    const h = await renderOrg('/org/integrations', { routes: { 'POST /api/v1/org/integrations': goIntegrationVersion } });
    await click(screen.getByRole('button', { name: 'Upload a bundle' }));
    expect(pickFiles).toHaveBeenCalledWith({ multiple: false, accept: '.zip,application/zip' });
    expect((lastBody('POST', '/api/v1/org/integrations') as FormData).get('file')).toBeInstanceOf(File);
    expect(h.router.state.location.pathname).toBe('/org/integrations/hubspot/versions/1.3.0');
    expect(screen.getByRole('button', { name: 'Approve and install' })).toBeInTheDocument();
  });

  // Guards the refusal (integration_upload_refused.json): a 400 that lists findings opens a dialog naming every
  // one, so the person can fix the bundle; a one-line banner would hide all but the headline.
  it('names every finding when a bundle does not pass the check', async () => {
    vi.mocked(pickFiles).mockResolvedValue([new File(['x'], 'bad.zip')]);
    const refused = goIntegrationUploadRefused;
    const h = await renderOrg('/org/integrations', { routes: { 'POST /api/v1/org/integrations': fail(400, refused.error, refused.who, refused) } });
    await click(screen.getByRole('button', { name: 'Upload a bundle' }));
    const dialog = within(screen.getByRole('dialog'));
    expect(dialog.getByText('The bundle did not pass the check')).toBeInTheDocument();
    for (const f of refused.findings) expect(dialog.getByText(f)).toBeInTheDocument();
    expect(h.router.state.location.pathname).toBe('/org/integrations');
  });

  // Guards every other refusal, such as a version that is already stored (409): the server's sentence shows in the
  // section's banner, and no findings dialog opens for a body that has none.
  it('says in a banner when the version is already stored', async () => {
    vi.mocked(pickFiles).mockResolvedValue([new File(['zip'], 'hubspot.zip')]);
    await renderOrg('/org/integrations', {
      routes: { 'POST /api/v1/org/integrations': fail(409, 'HubSpot already has version 1.3.0.', 'Raise the version in the bundle, then upload it again.') },
    });
    await click(screen.getByRole('button', { name: 'Upload a bundle' }));
    expect(screen.getByText('HubSpot already has version 1.3.0.')).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).toBeNull();
  });
});

describe('Integration install screen', () => {
  const PATH = '/org/integrations/hubspot/versions/1.3.0';
  const API = '/api/v1/org/integrations/hubspot/versions/1.3.0';

  // Guards the scopes a sign-in connection asks the service for, under the connection's fields: the owner approves
  // what the integration may do at the service before any sign-in. A version that names none (a token connection, or
  // GitHub's empty list) draws no line at all rather than an empty one.
  it('says what a sign-in asks the service for, when it asks for anything', async () => {
    await renderOrg(PATH, { routes: { ['GET ' + API]: { ...goIntegrationVersion, connection_method: 'device', scopes: ['repo', 'read:org'] } } });
    expect(within(screen.getByRole('region', { name: 'The connection needs' })).getByText('Asks the service for: repo, read:org')).toBeInTheDocument();
  });
  it('draws no scopes line for a version that names none', async () => {
    await renderOrg(PATH);
    expect(screen.queryByText(/^Asks the service for/)).toBeNull();
  });

  // Guards the review itself (integration_version.json): what it changes from the version in use, every host, each
  // connection field with Secret on the sensitive one, each action in words, what the access form asks, the bundle's
  // size, what it replaces and what the check noted. A fact the person would approve without seeing fails here.
  it('shows what the version reaches, holds and does before anything runs', async () => {
    const h = await renderOrg(PATH, { desktop: true });
    expect(h.chrome.at(-1)).toEqual({ title: 'HubSpot 1.3.0', back: { to: '/org/integrations/hubspot', label: 'HubSpot' } });
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('HubSpot 1.3.0');
    expect(screen.getByText('Needs your approval')).toHaveClass('kv-badge--signal');
    expect(within(screen.getByRole('region', { name: 'Who built it' })).getByText('Kivali Developer (agent)')).toBeInTheDocument();
    expect(within(screen.getByRole('region', { name: 'Version' })).getByText('Replaces version 1.2.0')).toBeInTheDocument();
    expect(within(screen.getByRole('region', { name: 'Version notes' })).getByText('Adds contacts. Reaches one more host.')).toBeInTheDocument();
    const changes = within(screen.getByRole('region', { name: 'What changes from version 1.2.0' }));
    expect(changes.getAllByRole('listitem').map((li) => li.textContent)).toEqual(goIntegrationVersion.changes);
    expect(within(screen.getByRole('region', { name: 'Reaches these hosts' })).getByText('api.hubapi.com')).toBeInTheDocument();

    const connection = within(screen.getByRole('region', { name: 'The connection needs' }));
    const token = connection.getByText('Private app token').closest('li') as HTMLElement;
    expect(within(token).getByText('Secret')).toBeInTheDocument();
    expect(within(token).getByText('Required · From HubSpot, Settings, Private apps.')).toBeInTheDocument();
    const portal = connection.getByText('Portal').closest('li') as HTMLElement;
    expect(within(portal).queryByText('Secret')).toBeNull();

    const actions = within(screen.getByRole('region', { name: 'Actions' }));
    expect(actions.getByText('Create post')).toBeInTheDocument();
    expect(actions.getByText('Create a blog post as a draft.')).toBeInTheDocument();
    expect(actions.getByText('List blogs')).toBeInTheDocument();

    const form = within(screen.getByRole('region', { name: 'Access form asks' }));
    expect(form.getByText('Any number of choices, listed by HubSpot · required · The blogs the agent may write in.')).toBeInTheDocument();
    expect(form.getByText('One choice: eu, us')).toBeInTheDocument();
    expect(form.getByText('On or off')).toBeInTheDocument();
    expect(form.getByText('A list of entries')).toBeInTheDocument();
    expect(form.getByText('Text')).toBeInTheDocument();

    expect(within(screen.getByRole('region', { name: 'Bundle' })).getByText('12 files, 180 KB')).toBeInTheDocument();
    expect(within(screen.getByRole('region', { name: 'The check noted' })).getByText('tools[2] (whoami) has no description')).toBeInTheDocument();
    expect(screen.getByText(/^The check only reads the bundle\./)).toBeInTheDocument();
  });

  // Guards a first version and an integration without an access form: with nothing approved yet the way back is
  // the list (the integration's page has nothing to run), and the screen says access is all or nothing rather
  // than drawing an empty list.
  it('goes back to the list for a first version, and says when access is all or nothing', async () => {
    const first = { ...goIntegrationVersion, current_version: undefined, access_form: undefined, findings: [] };
    const h = await renderOrg(PATH, { routes: { ['GET ' + API]: first } });
    expect(h.chrome.at(-1)).toEqual({ title: 'HubSpot 1.3.0', back: { to: '/org/integrations', label: 'Integrations' } });
    expect(within(screen.getByRole('region', { name: 'Access form asks' })).getByText('Access is all or nothing.')).toBeInTheDocument();
    expect(screen.queryByText(/^Replaces version/)).toBeNull();
    expect(screen.queryByRole('region', { name: /^What changes from version/ })).toBeNull();
    expect(screen.queryByRole('region', { name: 'The check noted' })).toBeNull();
  });

  // Guards the software a version's pod installs (integration_version.json's packages): it runs as root at every
  // start, so each name is listed with where it comes from and when it is installed, and the person is told to read
  // them. A version that installs nothing, and a built-in one, which installs nothing, draw no such block.
  it('lists the software a version installs, and draws nothing when it installs none', async () => {
    const h = await renderOrg(PATH);
    const software = within(screen.getByRole('region', { name: 'Software it installs' }));
    expect(software.getAllByRole('listitem').map((li) => li.textContent)).toEqual(goIntegrationVersion.packages);
    expect(software.getByText(/^Debian packages, fetched from Debian’s repositories and installed each time the integration starts\./)).toBeInTheDocument();
    expect(software.getByText(/Read each one before you approve\.$/)).toBeInTheDocument();
    h.unmount();

    await renderOrg(PATH, { routes: { ['GET ' + API]: { ...goIntegrationVersion, packages: [] } } });
    expect(screen.queryByRole('region', { name: 'Software it installs' })).toBeNull();
  });

  // Guards the empty diff: a later version that changes none of what the screen lists says so in one line, rather
  // than an empty block the person might read as not loaded.
  it('says when a later version changes no host, software, secret, action or access field', async () => {
    await renderOrg(PATH, { routes: { ['GET ' + API]: { ...goIntegrationVersion, changes: [] } } });
    const changes = within(screen.getByRole('region', { name: 'What changes from version 1.2.0' }));
    expect(changes.getByText('No change to hosts, software, secrets, actions or the access form.')).toBeInTheDocument();
    expect(changes.queryByRole('listitem')).toBeNull();
  });

  // Guards rolling back from a superseded version's screen: Roll back is its one button (no Approve, no Reject), it
  // opens the dialog naming the version that stops, the post waits for the confirm, and the screen then opens the
  // integration's page, where any access that no longer fits is marked.
  it('rolls back to a superseded version from its screen', async () => {
    const OLD = '/api/v1/org/integrations/hubspot/versions/1.1.0';
    const superseded = { ...goIntegrationVersion, version: '1.1.0', state: 'superseded', notes_md: 'Blog posts only.' };
    const h = await renderOrg('/org/integrations/hubspot/versions/1.1.0', {
      routes: { ['GET ' + OLD]: superseded, ['POST ' + OLD + '/rollback']: { ...superseded, state: 'approved', current: true, current_version: undefined, changes: [] } },
    });
    expect(screen.getByText('Replaced')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Approve and install' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Reject' })).toBeNull();
    await click(screen.getByRole('button', { name: 'Roll back' }));
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByText('Roll back to version 1.1.0?')).toBeInTheDocument();
    expect(within(dialog).getByText(/^Version 1\.2\.0 stops and 1\.1\.0 runs in its place\./)).toBeInTheDocument();
    expect(called('POST', OLD + '/rollback')).toBe(false);
    await click(within(dialog).getByRole('button', { name: 'Roll back' }));
    expect(called('POST', OLD + '/rollback')).toBe(true);
    expect(h.router.state.location.pathname).toBe('/org/integrations/hubspot');
  });

  // Guards a built-in integration's install screen (integration_version_builtin.json): it says the integration ships
  // with Kivali in place of who published it, shows no bundle and no review banner about one, says its reach is the
  // hosts its connection names rather than "No hosts.", and offers nothing to press, in use or replaced, since its
  // versions come with Kivali and the server refuses every answer (409).
  it('says a built-in version ships with Kivali and offers no approve, reject or roll back', async () => {
    const BUILTIN = '/api/v1/org/integrations/git/versions/1.0.0';
    const h = await renderOrg('/org/integrations/git/versions/1.0.0', { routes: { ['GET ' + BUILTIN]: goIntegrationVersionBuiltin } });
    expect(h.chrome.at(-1)).toEqual({ title: 'Git 1.0.0', back: { to: '/org/integrations/git', label: 'Git' } });
    const who = within(screen.getByRole('region', { name: 'Who built it' }));
    expect(who.getByText('Kivali')).toBeInTheDocument();
    expect(who.getByText('Ships with Kivali')).toBeInTheDocument();
    expect(who.queryByText(/^Published by/)).toBeNull();
    expect(screen.getByText('This integration ships with Kivali. It needs no install, and Kivali’s updates bring its new versions.')).toBeInTheDocument();
    expect(screen.queryByText(/^The check only reads the bundle\./)).toBeNull();
    expect(screen.queryByRole('region', { name: 'Bundle' })).toBeNull();
    const hosts = within(screen.getByRole('region', { name: 'Reaches these hosts' }));
    expect(hosts.getByText('Reaches the hosts its connection names.')).toBeInTheDocument();
    expect(hosts.queryByText('No hosts.')).toBeNull();
    expect(screen.queryAllByRole('button')).toEqual([]);
    h.unmount();

    // A replaced built-in version: nothing replaces the version in use by approving it, so neither the line nor the
    // block that weighs a replacement is drawn, though the server names the version in use and what differs.
    await renderOrg('/org/integrations/git/versions/1.0.0', {
      routes: {
        ['GET ' + BUILTIN]: { ...goIntegrationVersionBuiltin, state: 'superseded', current: false, current_version: '1.1.0', changes: ['Actions added: Push'] },
      },
    });
    expect(screen.getByText('Replaced')).toBeInTheDocument();
    expect(screen.queryByText(/^Replaces version/)).toBeNull();
    expect(screen.queryByRole('region', { name: /^What changes from version/ })).toBeNull();
    expect(screen.queryByText('Actions added: Push')).toBeNull();
    expect(screen.queryAllByRole('button')).toEqual([]);
  });

  // Guards the install screen of a setup connection (integration_version_setup.json): the flow the integration runs
  // is held to what the manifest declares, and those declarations are the review. Before approving, the owner reads
  // what setup does, every host it may send the browser to, every field it may ask for (secrets and values used once
  // marked) and every value it keeps; the connection block says setup makes the connection rather than "Nothing to
  // enter.", which would read as a connection with nothing behind it.
  it('lists what a setup flow may do on the install screen', async () => {
    const SETUP = '/api/v1/org/integrations/github/versions/2.0.0';
    await renderOrg('/org/integrations/github/versions/2.0.0', { routes: { ['GET ' + SETUP]: goIntegrationVersionSetup } });
    const setup = within(screen.getByRole('region', { name: 'Setup' }));
    const declared = goIntegrationVersionSetup.setup!;
    expect(setup.getByText(declared.summary)).toBeInTheDocument();
    expect(within(setup.getByRole('list', { name: 'Sends your browser to' })).getAllByRole('listitem').map((li) => li.textContent)).toEqual(declared.hosts);
    const asks = within(setup.getByRole('list', { name: 'GitHub asks for' }));
    for (const f of declared.fields) {
      expect(asks.getByText(f.title)).toBeInTheDocument();
      expect(asks.getByText(f.words)).toBeInTheDocument();
    }
    expect(asks.getAllByText('Secret')).toHaveLength(declared.fields.filter((f) => f.secret).length);
    expect(asks.getAllByText('Used once, never kept')).toHaveLength(declared.fields.filter((f) => f.setup_only).length);
    const keeps = within(setup.getByRole('list', { name: 'Keeps' }));
    for (const k of declared.keeps) {
      expect(keeps.getByText(k.words)).toBeInTheDocument();
      expect(keeps.getByText(k.key)).toBeInTheDocument();
    }
    const needs = within(screen.getByRole('region', { name: 'The connection needs' }));
    expect(needs.getByText('Nothing to enter: setup makes the connection.')).toBeInTheDocument();
    // A setup built-in reaches its service from Kivali, so its hosts are listed rather than "the hosts its connection
    // names", which a setup connection has none of.
    const hosts = within(screen.getByRole('region', { name: 'Reaches these hosts' }));
    expect(hosts.getAllByRole('listitem').map((li) => li.textContent)).toEqual(goIntegrationVersionSetup.hosts);
    expect(hosts.queryByText('Reaches the hosts its connection names.')).toBeNull();
    cleanup();

    // A version with no setup draws no such block.
    await renderOrg('/org/integrations/hubspot/versions/1.3.0');
    expect(screen.queryByRole('region', { name: 'Setup' })).toBeNull();
  });

  // Guards the install screen of a sign-in whose client Kivali registers itself (integration_version_registered.json):
  // the owner pastes nothing, and the screen says so where a form would otherwise list the client id and secret,
  // while Signs in at still lists the hosts the discovered endpoints may be on, since that is the bound Kivali holds
  // the service's metadata to.
  it('says Kivali registers itself for a registered client', async () => {
    const REG = '/api/v1/org/integrations/zoominfo/versions/1.3.0';
    await renderOrg('/org/integrations/zoominfo/versions/1.3.0', { routes: { ['GET ' + REG]: goIntegrationVersionRegistered } });
    const needs = within(screen.getByRole('region', { name: 'The connection needs' }));
    expect(needs.getByText('Kivali registers itself with the service; there is nothing to paste.')).toBeInTheDocument();
    const hosts = within(screen.getByRole('region', { name: 'Signs in at' }));
    for (const h of goIntegrationVersionRegistered.sign_in_hosts) expect(hosts.getByText(h)).toBeInTheDocument();
  });

  // Guards the install screen of an integration that ships with Kivali as a bundle (integration_version_shipped.json):
  // it runs in a pod of its own and reaches only the hosts its manifest lists, so the owner must see those hosts and
  // the bundle's size as for an upload, not "Reaches the hosts its connection names.", while it still says it ships
  // with Kivali and offers nothing to press, since the server refuses every answer to a built-in (409).
  it('shows a shipped bundle\'s hosts and bundle, says it ships with Kivali and offers nothing to press', async () => {
    await renderOrg('/org/integrations/notes/versions/1.0.0', {
      routes: { ['GET /api/v1/org/integrations/notes/versions/1.0.0']: goIntegrationVersionShipped },
    });
    const who = within(screen.getByRole('region', { name: 'Who built it' }));
    expect(who.getByText('Ships with Kivali')).toBeInTheDocument();
    expect(screen.getByText('This integration ships with Kivali. It needs no install, and Kivali’s updates bring its new versions.')).toBeInTheDocument();
    const hosts = within(screen.getByRole('region', { name: 'Reaches these hosts' }));
    expect(hosts.getByText('api.notes.example.com')).toBeInTheDocument();
    expect(hosts.queryByText('Reaches the hosts its connection names.')).toBeNull();
    const bundle = within(screen.getByRole('region', { name: 'Bundle' }));
    expect(bundle.getByText(bundleLine(3, 4096))).toBeInTheDocument();
    expect(screen.queryAllByRole('button')).toEqual([]);
  });

  // Guards the software of a shipped bundle such as GitHub, whose pod installs gh as root at every start as an
  // upload's does: its install screen lists each package, as an upload's does, without asking the owner to read them
  // before approving, since a shipped bundle has nothing to approve.
  it('lists the software a shipped bundle installs, with nothing to approve', async () => {
    await renderOrg('/org/integrations/notes/versions/1.0.0', {
      routes: { ['GET /api/v1/org/integrations/notes/versions/1.0.0']: { ...goIntegrationVersionShipped, packages: ['gh'] } },
    });
    const software = within(screen.getByRole('region', { name: 'Software it installs' }));
    expect(software.getAllByRole('listitem').map((li) => li.textContent)).toEqual(['gh']);
    expect(software.getByText('Debian packages, fetched from Debian’s repositories and installed each time the integration starts.')).toBeInTheDocument();
  });

  // Guards a rejected version's screen: it says Rejected and offers nothing to press, since a rejected version never
  // runs and cannot be rolled back to.
  it('shows a rejected version with no buttons', async () => {
    await renderOrg(PATH, { routes: { ['GET ' + API]: { ...goIntegrationVersion, state: 'rejected' } } });
    expect(screen.getByText('Rejected')).toBeInTheDocument();
    expect(screen.queryAllByRole('button')).toEqual([]);
  });

  // Guards approval: nothing is posted until Approve and install is clicked, which posts .../approve and lands on
  // the integration's page, where the connection is made.
  it('approves and opens the integration’s page', async () => {
    const h = await renderOrg(PATH, { routes: { ['POST ' + API + '/approve']: { ...goIntegrationVersion, state: 'approved', current: true } } });
    expect(called('POST', API + '/approve')).toBe(false);
    await click(screen.getByRole('button', { name: 'Approve and install' }));
    expect(called('POST', API + '/approve')).toBe(true);
    expect(h.router.state.location.pathname).toBe('/org/integrations/hubspot');
  });

  // Guards rejection: Reject only opens a danger dialog; the post waits for its confirm, and the screen then shows
  // the version as rejected with no buttons left to press.
  it('asks before rejecting, then shows the version rejected', async () => {
    await renderOrg(PATH, { routes: { ['POST ' + API + '/reject']: { ...goIntegrationVersion, state: 'rejected' } } });
    await click(screen.getByRole('button', { name: 'Reject' }));
    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveClass('kv-dialog--danger');
    expect(within(dialog).getByText('Reject version 1.3.0?')).toBeInTheDocument();
    expect(called('POST', API + '/reject')).toBe(false);
    await click(within(dialog).getByRole('button', { name: 'Reject' }));
    expect(called('POST', API + '/reject')).toBe(true);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByText('Rejected')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Approve and install' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Reject' })).toBeNull();
  });
});

describe('Integration page', () => {
  const PATH = '/org/integrations/hubspot';
  const API = '/api/v1/org/integrations/hubspot';

  // Guards the switch: turning it off posts .../disable and on posts .../enable, and the page shows what each
  // answer says (the status follows), so a switch that only moves on screen fails here.
  it('turns the integration off and on with its switch', async () => {
    const off = { ...goIntegrationPage, summary: { ...goIntegrationPage.summary, enabled: false, status: 'off' } };
    const h = await renderOrg(PATH, { routes: { ['POST ' + API + '/disable']: off, ['POST ' + API + '/enable']: goIntegrationPage } });
    expect(h.chrome.at(-1)).toEqual({ title: 'HubSpot', back: { to: '/org/integrations', label: 'Integrations' } });
    const sw = screen.getByRole('switch', { name: 'On' });
    expect(sw).toHaveAttribute('aria-checked', 'true');
    await click(sw);
    expect(called('POST', API + '/disable')).toBe(true);
    expect(sw).toHaveAttribute('aria-checked', 'false');
    expect(screen.getByText('Off')).toHaveClass('kv-badge');
    await click(sw);
    expect(called('POST', API + '/enable')).toBe(true);
    expect(sw).toHaveAttribute('aria-checked', 'true');
  });

  // Guards ConnectionWrite's secret rule: a stored secret shows only "Stored", a secret left blank is not sent (so
  // it keeps its value; sending "" would clear it), and a typed one is. The body is checked against
  // connection_write.json, the shape the Go server reads.
  it('saves the connection with only the secrets that were typed', async () => {
    await renderOrg(PATH, { routes: { ['PUT ' + API + '/connection']: goIntegrationPage } });
    const token = screen.getByLabelText('Private app token');
    expect(token).toHaveAttribute('type', 'password');
    expect(token).toHaveAttribute('placeholder', 'Stored');
    expect(token).toHaveValue('');
    expect(screen.getByLabelText('Portal (optional)')).toHaveValue('acme');
    await click(screen.getByRole('button', { name: 'Save connection' }));
    expect(lastBody('PUT', API + '/connection')).toEqual({ fields: { portal: 'acme' } });

    fireEvent.change(screen.getByLabelText('Private app token'), { target: { value: 'pat-eu1-...' } });
    await click(screen.getByRole('button', { name: 'Save connection' }));
    expect(lastBody('PUT', API + '/connection')).toEqual(goConnectionWrite);
    // Saved: the form starts again from what is stored, so the typed secret leaves the page.
    expect(screen.getByLabelText('Private app token')).toHaveValue('');
  });

  // Guards the connection's standing and Test connection: the lines say since when it is connected and what the
  // last test found, and a refused test (409 while testing is unavailable) shows the server's sentence instead of
  // failing silently.
  it('says how the connection stands, and why a test could not run', async () => {
    await renderOrg(PATH, { routes: { ['POST ' + API + '/connection/verify']: fail(409, 'Testing a connection is not available yet.', 'You can still save it.') } });
    expect(screen.getByText(/^Connected since /)).toBeInTheDocument();
    expect(screen.getByText(/^Verified .*: Connected as Acme Marketing$/)).toBeInTheDocument();
    await click(screen.getByRole('button', { name: 'Test connection' }));
    expect(called('POST', API + '/connection/verify')).toBe(true);
    expect(screen.getByText('Testing a connection is not available yet.')).toBeInTheDocument();
  });

  // Guards the two states where the form cannot be used as it is: secrets missing from this server (a danger banner
  // asks for them again) and no approved version yet (no form at all, nothing to connect).
  it('asks to reconnect when the secrets are missing, and waits for an approved version before any form', async () => {
    const lost = { ...goIntegrationPage, connection: { ...goIntegrationPage.connection!, reconnect_needed: true } };
    const h = await renderOrg(PATH, { routes: { ['GET ' + API]: lost } });
    expect(screen.getByText('Reconnect needed').closest('[role="alert"]')).toHaveClass('kv-banner--danger');
    h.unmount();

    const pending = { ...goIntegrationPage, connection: undefined, summary: { ...goIntegrationPage.summary, version: '', status: 'pending' } };
    await renderOrg(PATH, { routes: { ['GET ' + API]: pending, ['GET ' + API + '/audit']: { rows: [] } } });
    expect(screen.getByText('Approve a version first.')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Save connection' })).toBeNull();
    await click(screen.getByRole('button', { name: 'Recent calls' }));
    expect(screen.getByText('No calls yet.')).toBeInTheDocument();
  });

  // Guards Disconnect: the DELETE waits for the danger dialog's confirm, and the page then shows no connection, with
  // Connect in place of the form, which starts the flow that makes one again.
  it('asks before disconnecting', async () => {
    const disconnected = { ...goIntegrationPage, connection: undefined, summary: { ...goIntegrationPage.summary, status: 'not_connected' } };
    await renderOrg(PATH, { routes: { ['DELETE ' + API + '/connection']: disconnected } });
    await click(screen.getByRole('button', { name: 'Disconnect' }));
    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveClass('kv-dialog--danger');
    expect(within(dialog).getByText('Disconnect HubSpot?')).toBeInTheDocument();
    expect(called('DELETE', API + '/connection')).toBe(false);
    await click(within(dialog).getByRole('button', { name: 'Disconnect' }));
    expect(called('DELETE', API + '/connection')).toBe(true);
    expect(screen.getByRole('button', { name: 'Connect' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Test connection' })).toBeNull();
  });

  // Guards the access form as the bundle describes it (integration_page.json's five kinds): text is a text field,
  // choice a select, switch a checkbox, list an entry field, and choices a checkbox per option once Load options has
  // asked the integration for them. The PUT body is the filled form alone, with no connection id (there is one
  // connection); a kind drawn wrong, a live option never offered or a value sent in the wrong shape fails here.
  it('gives an agent access through a form drawn from the integration’s fields', async () => {
    await renderOrg(PATH, { routes: { ['GET ' + API + '/choices/blogs']: goAccessChoices, ['PUT ' + API + '/access/chief-of-staff']: goIntegrationPage } });
    const access = within(screen.getByRole('region', { name: 'Access' }));
    expect(access.getByRole('combobox', { name: 'Agent' })).toHaveValue('chief-of-staff');
    await click(access.getByRole('button', { name: 'Give access' }));
    const dialog = within(screen.getByRole('dialog'));
    expect(dialog.getByText('Give Chief of Staff access to HubSpot')).toBeInTheDocument();
    expect(dialog.getByRole('textbox', { name: 'Note (optional)' })).toBeInTheDocument();
    const region = dialog.getByRole('combobox', { name: 'Region (optional)' });
    expect(within(region).getAllByRole('option').map((o) => o.textContent)).toEqual(['Not set', 'eu', 'us']);
    expect(dialog.getByRole('checkbox', { name: 'May publish and edit' })).toHaveAttribute('aria-checked', 'false');
    expect(dialog.getByRole('textbox', { name: 'Tags (optional)' })).toBeInTheDocument();
    // Blogs lists its options live, through the connection: none until asked.
    expect(dialog.queryByRole('checkbox', { name: 'Marketing' })).toBeNull();
    await click(dialog.getByRole('button', { name: 'Load options' }));
    expect(called('GET', API + '/choices/blogs')).toBe(true);
    expect(dialog.getByRole('checkbox', { name: 'Engineering' })).toBeInTheDocument();
    await click(dialog.getByRole('checkbox', { name: 'Marketing' }));
    await click(dialog.getByRole('checkbox', { name: 'May publish and edit' }));
    await click(dialog.getByRole('button', { name: 'Give access' }));
    expect(lastBody('PUT', API + '/access/chief-of-staff')).toEqual({ access: { blogs: ['Marketing'], write: true, delete: false } });
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  // Guards a switch the integration marks dangerous (integration_page.json's delete, as Git's force-push is): it is
  // drawn in the warning tone with its description beside a warning icon, not as a muted hint, while an ordinary
  // switch keeps the plain look; and it still sends true or false like any switch. A danger flag dropped on the way to
  // the dialog would draw a force-push as quietly as any other choice.
  it('draws a dangerous switch in the warning tone with what it does in view', async () => {
    await renderOrg(PATH, { routes: { ['PUT ' + API + '/access/chief-of-staff']: goIntegrationPage } });
    await click(within(screen.getByRole('region', { name: 'Access' })).getByRole('button', { name: 'Give access' }));
    const dialog = within(screen.getByRole('dialog'));
    const risky = dialog.getByRole('checkbox', { name: 'May delete posts' });
    const box = risky.closest('.app-int-danger') as HTMLElement;
    expect(box).not.toBeNull();
    const note = within(box).getByText('Deletes posts for good.');
    expect(note.closest('p')).toHaveClass('kv-text--signal');
    expect(dialog.getByRole('checkbox', { name: 'May publish and edit' }).closest('.app-int-danger')).toBeNull();
    await click(risky);
    await click(dialog.getByRole('button', { name: 'Give access' }));
    expect(lastBody('PUT', API + '/access/chief-of-staff')).toEqual({ access: { write: false, delete: true } });
  });

  // Guards Change and its failures: the form starts from the agent's access as it stands, a list field adds entries
  // as removable chips, a refused options lookup (409) says why inline, and a 400 keeps the dialog open with the
  // server's sentence in it, so nothing the person typed is lost.
  it('changes an agent’s access, and shows a refusal in the dialog', async () => {
    await renderOrg(PATH, {
      routes: {
        ['GET ' + API + '/choices/blogs']: fail(409, 'Connect HubSpot before listing its options.', 'you'),
        ['PUT ' + API + '/access/dana']: fail(400, 'Blogs needs at least one choice.', 'you'),
      },
    });
    const table = within(screen.getByRole('region', { name: 'Access' })).getByRole('table');
    expect(within(table).getByText('Blogs: Marketing · May publish and edit')).toBeInTheDocument();
    expect(within(table).getByText(/^Invalid: Blogs lists a choice the form no longer offers/)).toHaveClass('kv-text--danger');

    await click(screen.getByRole('button', { name: 'Change Dana’s access' }));
    const dialog = within(screen.getByRole('dialog'));
    expect(dialog.getByText('Change Dana’s access to HubSpot')).toBeInTheDocument();
    expect(dialog.getByRole('checkbox', { name: 'Marketing' })).toHaveAttribute('aria-checked', 'true');
    expect(dialog.getByRole('checkbox', { name: 'May publish and edit' })).toHaveAttribute('aria-checked', 'true');
    await click(dialog.getByRole('button', { name: 'Load options' }));
    expect(dialog.getByText('Connect HubSpot before listing its options.')).toBeInTheDocument();

    fireEvent.change(dialog.getByRole('textbox', { name: 'Tags (optional)' }), { target: { value: 'launch' } });
    await click(dialog.getByRole('button', { name: 'Add' }));
    expect(dialog.getByRole('button', { name: 'Remove launch' })).toBeInTheDocument();
    await click(dialog.getByRole('checkbox', { name: 'Marketing' }));
    await click(dialog.getByRole('button', { name: 'Save access' }));
    expect(lastBody('PUT', API + '/access/dana')).toEqual({ access: { write: true, delete: false, tags: ['launch'] } });
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(dialog.getByText('Blogs needs at least one choice.')).toBeInTheDocument();
  });

  // Guards an integration whose access is all or nothing: the dialog says so and Approve sends an empty form ({}),
  // the body AccessWrite asks for, rather than a form with nothing in it, and no connection id: with one connection the server uses it.
  it('grants all-or-nothing access with an empty form', async () => {
    const plain = { ...goIntegrationPage, access_form: undefined };
    await renderOrg(PATH, { routes: { ['GET ' + API]: plain, ['PUT ' + API + '/access/chief-of-staff']: plain } });
    await click(screen.getByRole('button', { name: 'Give access' }));
    const dialog = within(screen.getByRole('dialog'));
    expect(dialog.getByText('This integration’s access is all or nothing.')).toBeInTheDocument();
    await click(dialog.getByRole('button', { name: 'Approve' }));
    expect(lastBody('PUT', API + '/access/chief-of-staff')).toEqual({ access: {} });
  });

  // Guards taking access away: Remove beside an agent deletes that agent's access and the table follows the answer.
  it('removes an agent’s access', async () => {
    const without = { ...goIntegrationPage, access: [goIntegrationPage.access[1]!] };
    await renderOrg(PATH, { routes: { ['DELETE ' + API + '/access/dana']: without } });
    await click(screen.getByRole('button', { name: 'Remove Dana’s access' }));
    expect(called('DELETE', API + '/access/dana')).toBe(true);
    expect(screen.queryByRole('button', { name: 'Change Dana’s access' })).toBeNull();
  });

  // Guards not_running: the status reads "Not running" with its caption, and nothing that needs the process (Test
  // connection, the access dialog's Load options) can be pressed, so a click cannot reach an integration that is down.
  it('says when the integration is not running and disables what needs it', async () => {
    const down = { ...goIntegrationPage, summary: { ...goIntegrationPage.summary, status: 'not_running' } };
    await renderOrg(PATH, { routes: { ['GET ' + API]: down } });
    expect(screen.getByText('Not running')).toHaveClass('kv-badge');
    expect(screen.getByText('Kivali is starting it, or it stopped. The log below says what happened.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Test connection' })).toBeDisabled();
    await click(screen.getByRole('button', { name: 'Give access' }));
    const dialog = within(screen.getByRole('dialog'));
    for (const b of dialog.getAllByRole('button', { name: 'Load options' })) expect(b).toBeDisabled();
    expect(dialog.getAllByText('HubSpot is not running, so it cannot list these.').length).toBeGreaterThan(0);
  });

  // Guards the two answers of Test connection: a good one shows "Verified ...: message" as the status line; a bad
  // one (verified false) shows its message in a danger banner rather than as a quiet line.
  it('shows what Test connection found, good or bad', async () => {
    const conn = goIntegrationPage.connection!;
    const good = { ...goIntegrationPage, connection: { ...conn, verified: true, verify_message: 'Connected as Acme' } };
    const bad = { ...goIntegrationPage, connection: { ...conn, verified: false, verify_message: 'The token was refused.' } };
    let answerWith: unknown = good;
    await renderOrg(PATH, { routes: { ['POST ' + API + '/connection/verify']: () => answerWith } });
    await click(screen.getByRole('button', { name: 'Test connection' }));
    expect(screen.getByText(/^Verified .*: Connected as Acme$/)).toBeInTheDocument();
    answerWith = bad;
    await click(screen.getByRole('button', { name: 'Test connection' }));
    expect(screen.getByText('The token was refused.').closest('[role="alert"]')).toHaveClass('kv-banner--danger');
    expect(screen.queryByText(/^Verified /)).toBeNull();
  });

  // Guards an integration with no connection test (connection_test false: its manifest names no kivali.verify). The
  // page must offer no Test connection, which the server can only refuse, and must not say it is unavailable while
  // the integration is not running. Save connection and Disconnect stay.
  it('offers no Test connection to an integration without one', async () => {
    const untested = { ...goIntegrationPage, connection_test: false, summary: { ...goIntegrationPage.summary, status: 'not_running' as const } };
    await renderOrg(PATH, { routes: { ['GET ' + API]: untested } });
    expect(screen.getByRole('button', { name: 'Save connection' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Disconnect' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Test connection' })).toBeNull();
    expect(screen.queryByText('Not available while it is not running.')).toBeNull();
  });

  // Guards the page's Software block: a version in use that installs software lists each package and says it is
  // installed each time the integration starts, so the owner can see what runs as root at every start without
  // opening the version's install screen. One that installs none draws no block.
  it('lists the software the version in use installs, and draws nothing when it installs none', async () => {
    const h = await renderOrg(PATH, { routes: { ['GET ' + API]: { ...goIntegrationPage, packages: ['gh', 'jq'] } } });
    const software = within(screen.getByRole('region', { name: 'Software' }));
    expect(software.getAllByRole('listitem').map((li) => li.textContent)).toEqual(['gh', 'jq']);
    expect(software.getByText('Debian packages, installed each time the integration starts.')).toBeInTheDocument();
    h.unmount();

    await renderOrg(PATH);
    expect(goIntegrationPage.packages).toEqual([]);
    expect(screen.queryByRole('region', { name: 'Software' })).toBeNull();
  });

  // Guards the Software block of an integration that ships with Kivali as a bundle, such as GitHub with gh: it is
  // built in, and its pod still installs software as root at every start, so the page lists it as for an upload.
  it('lists the software of a built-in that ships as a bundle', async () => {
    const page = { ...goIntegrationPage, summary: { ...goIntegrationPage.summary, builtin: true }, packages: ['gh'] };
    await renderOrg(PATH, { routes: { ['GET ' + API]: page } });
    const software = within(screen.getByRole('region', { name: 'Software' }));
    expect(software.getAllByRole('listitem').map((li) => li.textContent)).toEqual(['gh']);
  });

  // Guards the debug level, which apt-get's output has while an integration's software installs: it gets the
  // quietest badge (outline), so those lines read as detail beside stderr, warning and error.
  it('marks a debug log line with a quiet badge', async () => {
    const logs = { lines: [{ at: '2026-10-21T09:30:00Z', level: 'debug', message: 'Setting up gh ...' }] };
    await renderOrg(PATH, { routes: { ['GET ' + API + '/logs']: logs } });
    await click(screen.getByRole('button', { name: 'Recent logs' }));
    const block = within(screen.getByRole('region', { name: 'Recent logs' }));
    expect(block.getByText('Setting up gh ...')).toBeInTheDocument();
    expect(block.getByText('debug')).toHaveClass('kv-badge--outline');
  });

  // Guards the log block (integration_logs.json): lines show time, a level badge (none for info) and the message,
  // an empty log says so, and Refresh asks the server again and shows the new answer.
  it('shows the recent logs and refreshes them', async () => {
    let logs: unknown = goIntegrationLogs;
    await renderOrg(PATH, { routes: { ['GET ' + API + '/logs']: () => logs } });
    await click(screen.getByRole('button', { name: 'Recent logs' }));
    const block = within(screen.getByRole('region', { name: 'Recent logs' }));
    expect(block.getByText('server started')).toBeInTheDocument();
    expect(block.getByText('token [secret] accepted')).toBeInTheDocument();
    expect(block.getByText('stderr')).toHaveClass('kv-badge');
    expect(block.queryByText('info')).toBeNull();
    logs = { lines: [] };
    await click(block.getByRole('button', { name: 'Refresh' }));
    expect(block.getByText('Nothing logged yet.')).toBeInTheDocument();
    expect(block.queryByText('server started')).toBeNull();
  });

  // Guards the page's length: Recent calls and Recent logs start closed, so a page with a long history opens on its
  // switch, connection and access. Each heading ends in a toggle, named for its block, that says whether the block is
  // open; Refresh shows only while the log is open, and closing a block hides its rows again.
  it('starts Recent calls and Recent logs closed, and opens and closes each from its toggle', async () => {
    await renderOrg(PATH);
    for (const name of ['Recent calls', 'Recent logs']) {
      expect(screen.getByRole('button', { name })).toHaveAttribute('aria-expanded', 'false');
    }
    expect(screen.queryByText('812 ms')).toBeNull();
    expect(screen.queryByText('server started')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Refresh' })).toBeNull();

    await click(screen.getByRole('button', { name: 'Recent calls' }));
    expect(screen.getByRole('button', { name: 'Recent calls' })).toHaveAttribute('aria-expanded', 'true');
    expect(within(screen.getByRole('region', { name: 'Recent calls' })).getByText('812 ms')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Recent logs' })).toHaveAttribute('aria-expanded', 'false');

    await click(screen.getByRole('button', { name: 'Recent logs' }));
    const logs = within(screen.getByRole('region', { name: 'Recent logs' }));
    expect(logs.getByText('server started')).toBeInTheDocument();
    expect(logs.getByRole('button', { name: 'Refresh' })).toBeInTheDocument();

    await click(screen.getByRole('button', { name: 'Recent calls' }));
    expect(screen.getByRole('button', { name: 'Recent calls' })).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByText('812 ms')).toBeNull();
  });

  // Guards Remove: the DELETE waits for the danger dialog's confirm, then the page goes back to the list; a built-in
  // integration (can_remove false) offers no Remove at all, since the server would refuse it.
  it('removes the integration after asking, and never offers it for a built-in one', async () => {
    const h = await renderOrg(PATH, { routes: { ['DELETE ' + API]: null } });
    await click(screen.getByRole('button', { name: 'Remove' }));
    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveClass('kv-dialog--danger');
    expect(within(dialog).getByText('Remove HubSpot?')).toBeInTheDocument();
    expect(within(dialog).getByText('Its connection and every agent’s access go with it. A new bundle can install it again.')).toBeInTheDocument();
    expect(called('DELETE', API)).toBe(false);
    await click(within(dialog).getByRole('button', { name: 'Remove' }));
    expect(called('DELETE', API)).toBe(true);
    expect(h.router.state.location.pathname).toBe('/org/integrations');
    h.unmount();

    const builtin = { ...goIntegrationPage, can_remove: false, summary: { ...goIntegrationPage.summary, builtin: true } };
    await renderOrg(PATH, { routes: { ['GET ' + API]: builtin } });
    expect(screen.getByText('Built in')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Remove' })).toBeNull();
  });

  // Guards the history blocks: the waiting version's banner leads to its install screen, versions are listed newest
  // first with their state (integration_page.json has one of each), each linking to its install screen, only the
  // superseded one offers Roll back, and the recent calls say who called, what, and how it went
  // (integration_audit.json), with your roll back named as one rather than read as a failed call.
  it('shows the waiting version, every version and the recent calls', async () => {
    const h = await renderOrg(PATH);
    const versions = within(screen.getByRole('region', { name: 'Versions' }));
    const links = versions.getAllByRole('link');
    expect(links.map((l) => l.getAttribute('href'))).toEqual([
      '/org/integrations/hubspot/versions/1.3.0',
      '/org/integrations/hubspot/versions/1.2.0',
      '/org/integrations/hubspot/versions/1.1.1',
      '/org/integrations/hubspot/versions/1.1.0',
    ]);
    expect(within(links[0]!).getByText('Needs your approval')).toBeInTheDocument();
    expect(within(links[0]!).getByText('Adds contacts.')).toBeInTheDocument();
    expect(within(links[1]!).getByText('In use')).toHaveClass('kv-badge--success');
    expect(within(links[2]!).getByText('Rejected')).toBeInTheDocument();
    expect(within(links[3]!).getByText('Replaced')).toBeInTheDocument();
    // Roll back sits beside the superseded version's link, never inside one, and nowhere else.
    expect(versions.getAllByRole('button').map((b) => b.getAttribute('aria-label'))).toEqual(['Roll back to version 1.1.0']);
    for (const l of links) expect(within(l).queryByRole('button')).toBeNull();

    await click(screen.getByRole('button', { name: 'Recent calls' }));
    const calls = within(screen.getByRole('region', { name: 'Recent calls' }));
    expect(calls.getAllByText('Create post')).toHaveLength(2);
    expect(calls.getByText('Allowed')).toBeInTheDocument();
    expect(calls.getByText('Refused: Lee has no access to the Marketing blog')).toBeInTheDocument();
    expect(calls.getByText('812 ms')).toBeInTheDocument();
    const rollback = calls.getByText('Roll back').closest('tr') as HTMLElement;
    expect(within(rollback).getByText('From version 1.2.0 to version 1.1.0')).toBeInTheDocument();
    expect(within(rollback).getByText('Evan')).toBeInTheDocument();
    expect(calls.queryByText(/^Failed: from version/)).toBeNull();

    await click(screen.getByRole('button', { name: 'Review' }));
    expect(h.router.state.location.pathname).toBe('/org/integrations/hubspot/versions/1.3.0');
  });

  // Guards rolling back from the page: Roll back only opens the dialog, which names the version that stops and the one
  // that runs and says access stays as it is; the post waits for its confirm, then the page and the recent calls are
  // read again, so the list shows the version rolled back to in use and Roll back moves to the version it replaced.
  it('rolls back to a superseded version after asking, and the versions list follows', async () => {
    const versionsAfter = goIntegrationPage.versions.map((v) =>
      v.version === '1.1.0' ? { ...v, state: 'approved' } : v.version === '1.2.0' ? { ...v, state: 'superseded' } : v,
    );
    const rolledBack = { ...goIntegrationPage, summary: { ...goIntegrationPage.summary, version: '1.1.0' }, versions: versionsAfter };
    let pageNow: unknown = goIntegrationPage;
    await renderOrg(PATH, {
      routes: {
        ['GET ' + API]: () => pageNow,
        ['POST ' + API + '/versions/1.1.0/rollback']: () => {
          pageNow = rolledBack;
          return { ...goIntegrationVersion, version: '1.1.0', state: 'approved', current: true, current_version: undefined, changes: [] };
        },
      },
    });
    const auditReads = () => calls.filter((c) => c.method === 'GET' && c.url === API + '/audit').length;
    const readsBefore = auditReads();
    await click(screen.getByRole('button', { name: 'Roll back to version 1.1.0' }));
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByText('Roll back to version 1.1.0?')).toBeInTheDocument();
    expect(
      within(dialog).getByText('Version 1.2.0 stops and 1.1.0 runs in its place. Agents’ access stays as it is; rows that no longer fit 1.1.0’s access form are marked.'),
    ).toBeInTheDocument();
    expect(called('POST', API + '/versions/1.1.0/rollback')).toBe(false);
    await click(within(dialog).getByRole('button', { name: 'Roll back' }));
    expect(called('POST', API + '/versions/1.1.0/rollback')).toBe(true);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(within(document.querySelector('header.app-int-head') as HTMLElement).getByText('Version 1.1.0')).toBeInTheDocument();
    const versions = within(screen.getByRole('region', { name: 'Versions' }));
    const links = versions.getAllByRole('link');
    expect(within(links[3]!).getByText('In use')).toBeInTheDocument();
    expect(within(links[1]!).getByText('Replaced')).toBeInTheDocument();
    expect(versions.getAllByRole('button').map((b) => b.getAttribute('aria-label'))).toEqual(['Roll back to version 1.2.0']);
    expect(auditReads()).toBe(readsBefore + 1);
  });

  // Guards a built-in integration's page: it is never not running (it runs inside Kivali), so Test connection works
  // as soon as there is a connection and nothing says it is not running; its versions offer no Roll back, even a
  // replaced one, since its versions come with Kivali and the server refuses a roll back (409).
  it('lets a built-in integration be tested once connected, and offers no roll back', async () => {
    const builtin = {
      ...goIntegrationPage,
      can_remove: false,
      summary: { ...goIntegrationPage.summary, builtin: true, pending_version: undefined },
      versions: goIntegrationPage.versions.filter((v) => v.state === 'approved' || v.state === 'superseded'),
    };
    await renderOrg(PATH, { routes: { ['GET ' + API]: builtin, ['POST ' + API + '/connection/verify']: builtin } });
    expect(screen.getByText('Built in')).toBeInTheDocument();
    expect(screen.queryByText(/not running/i)).toBeNull();
    expect(screen.queryByText(/waits for your approval/)).toBeNull();
    const test = screen.getByRole('button', { name: 'Test connection' });
    expect(test).toBeEnabled();
    await click(test);
    expect(called('POST', API + '/connection/verify')).toBe(true);
    const versions = within(screen.getByRole('region', { name: 'Versions' }));
    expect(versions.getByText('Replaced')).toBeInTheDocument();
    expect(versions.queryAllByRole('button')).toEqual([]);
    expect(screen.queryByRole('button', { name: 'Remove' })).toBeNull();
  });

  // Guards a built-in record this version of Kivali does not carry (status not_included): nothing runs it, so the
  // page must not say Kivali is starting it, Test connection waits, and Remove is offered (can_remove), since the
  // server removes such a record and the owner has no other way to take it off the team.
  it('says a built-in integration is not in this version of Kivali and lets it be removed', async () => {
    const stale = { ...goIntegrationPage, can_remove: true, summary: { ...goIntegrationPage.summary, builtin: true, status: 'not_included', pending_version: undefined } };
    await renderOrg(PATH, { routes: { ['GET ' + API]: stale, ['DELETE ' + API]: null } });
    expect(screen.getByText('Not in this version of Kivali')).toHaveClass('kv-badge');
    expect(screen.getByText('Another version of Kivali shipped it, and this one does not include it. Agents cannot use it; you can remove it.')).toBeInTheDocument();
    expect(screen.queryByText(/Kivali is starting it/)).toBeNull();
    expect(screen.getByRole('button', { name: 'Test connection' })).toBeDisabled();
    await click(screen.getByRole('button', { name: 'Remove' }));
    await click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(called('DELETE', API)).toBe(true);
  });

  // Guards a refused roll back (a 409, such as a version answered in another tab): the dialog stays open with the
  // server's sentence, so the click does not look like it worked.
  it('keeps the roll back dialog open with the refusal', async () => {
    await renderOrg(PATH, { routes: { ['POST ' + API + '/versions/1.1.0/rollback']: fail(409, 'version 1.1.0 of hubspot is the version in use', 'no one; reload to see what is waiting') } });
    await click(screen.getByRole('button', { name: 'Roll back to version 1.1.0' }));
    await click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Roll back' }));
    expect(within(screen.getByRole('dialog')).getByText(/version 1\.1\.0 of hubspot is the version in use/i)).toBeInTheDocument();
  });

  // Guards the banner over access that no longer fits the version in use: it counts the invalid rows in words (one,
  // or several), asks for them to be set again, and is gone when every row fits. integration_page.json has one.
  it('says how many access rows no longer fit the version in use', async () => {
    const one = '1 access row no longer fits this version’s access form. Set it again.';
    let h = await renderOrg(PATH);
    expect(screen.getByText(one).closest('[role="alert"]')).toHaveClass('kv-banner--warning');
    h.unmount();

    const invalid = goIntegrationPage.access[1]!.invalid;
    const both = { ...goIntegrationPage, access: goIntegrationPage.access.map((r) => ({ ...r, invalid })) };
    h = await renderOrg(PATH, { routes: { ['GET ' + API]: both } });
    expect(screen.getByText('2 access rows no longer fit this version’s access form. Set them again.')).toBeInTheDocument();
    h.unmount();

    const fits = { ...goIntegrationPage, access: [goIntegrationPage.access[0]!] };
    await renderOrg(PATH, { routes: { ['GET ' + API]: fits } });
    expect(screen.queryByText(/no longer fits? this version’s access form/)).toBeNull();
  });

  // Guards Setting the row again: a Change that the server accepts answers with the page, whose row is no longer
  // invalid, and the banner goes with it.
  it('clears the banner once the invalid row is set again', async () => {
    const fixed = { ...goIntegrationPage, access: goIntegrationPage.access.map((r) => ({ ...r, invalid: undefined })) };
    await renderOrg(PATH, { routes: { ['PUT ' + API + '/access/lee']: fixed } });
    expect(screen.getByText(/^1 access row no longer fits/)).toBeInTheDocument();
    await click(screen.getByRole('button', { name: 'Change Lee’s access' }));
    const dialog = within(screen.getByRole('dialog'));
    fireEvent.change(dialog.getByRole('textbox', { name: 'Tags (optional)' }), { target: { value: 'launch' } });
    await click(dialog.getByRole('button', { name: 'Add' }));
    await click(dialog.getByRole('button', { name: 'Save access' }));
    expect(called('PUT', API + '/access/lee')).toBe(true);
    expect(screen.queryByText(/^1 access row no longer fits/)).toBeNull();
    expect(screen.queryByText(/^Invalid: /)).toBeNull();
  });
});

describe('Integration page, several connections', () => {
  const PATH = '/org/integrations/git';
  const API = '/api/v1/org/integrations/git';
  const HUB = goIntegrationPageMultiple.connections[0]!;
  const LAB = goIntegrationPageMultiple.connections[1]!;
  // A connection's card, found by its title: a stored connection's is a <details>, folded until its title is clicked;
  // one being added is a plain card.
  const card = (name: string) => screen.getByText(name, { selector: '.kv-card-title' }).closest('.kv-card') as HTMLElement;
  const openCard = async (name: string) => {
    await click(screen.getByText(name, { selector: '.kv-card-title' }));
    return within(card(name));
  };
  const gitRoutes = (more: Record<string, Route> = {}) => ({
    ['GET ' + API]: goIntegrationPageMultiple,
    ['GET ' + API + '/audit']: { rows: [] },
    ['GET ' + API + '/logs']: { lines: [] },
    ...more,
  });

  // Guards the Connections block of an integration that takes several (integration_page_multiple.json, Git's page):
  // one card per connection, named by its label and filled with its own values, each saving to its own id, with no
  // Test connection since Git has none; and Add a connection opens a blank form with a label whose body is
  // connection_create.json. A card that saved through the singular route, or showed the first connection's host in
  // the second, would change the wrong account.
  it('draws a card per connection and saves and adds by id', async () => {
    await renderOrg(PATH, {
      routes: gitRoutes({
        ['PUT ' + API + '/connections/' + LAB.id]: goIntegrationPageMultiple,
        ['POST ' + API + '/connections']: goIntegrationPageMultiple,
      }),
    });
    const block = within(screen.getByRole('region', { name: 'Connections' }));
    expect(screen.queryByRole('region', { name: 'Connection' })).toBeNull();
    const hub = await openCard('GitHub');
    const lab = await openCard('Company GitLab');
    expect(hub.getByLabelText('Host')).toHaveValue('github.com');
    expect(lab.getByLabelText('Host')).toHaveValue('gitlab.example.com');
    expect(lab.getByLabelText('Label')).toHaveValue('Company GitLab');
    expect(lab.getByLabelText('Access token')).toHaveAttribute('placeholder', 'Stored');
    expect(lab.getByRole('alert')).toHaveTextContent('Reconnect needed');
    expect(hub.queryByText('Reconnect needed')).toBeNull();
    expect(block.queryByRole('button', { name: 'Test connection' })).toBeNull();
    expect(block.queryByText(/^Verified /)).toBeNull();

    fireEvent.change(lab.getByLabelText('Host'), { target: { value: 'gitlab.example.org' } });
    await click(lab.getByRole('button', { name: 'Save connection' }));
    // The label was not typed into, so it is not sent: one the integration derives keeps following the host.
    expect(lastBody('PUT', API + '/connections/' + LAB.id)).toEqual({ fields: { host: 'gitlab.example.org' } });
    expect(called('PUT', API + '/connection')).toBe(false);
    const labAgain = within(card('Company GitLab'));
    fireEvent.change(labAgain.getByLabelText('Label'), { target: { value: 'GitLab' } });
    await click(labAgain.getByRole('button', { name: 'Save connection' }));
    expect(lastBody('PUT', API + '/connections/' + LAB.id)).toEqual({ label: 'GitLab', fields: { host: 'gitlab.example.com' } });

    await click(block.getByRole('button', { name: 'Add a connection' }));
    const added = within(card('New connection'));
    expect(added.getByLabelText('Host')).toHaveValue('');
    expect(added.getByLabelText('Access token')).not.toHaveAttribute('placeholder');
    fireEvent.change(added.getByLabelText('Label'), { target: { value: 'Staging GitLab' } });
    fireEvent.change(added.getByLabelText('Host'), { target: { value: 'gitlab-staging.example.com' } });
    fireEvent.change(added.getByLabelText('Access token'), { target: { value: 'glpat-...' } });
    await click(added.getByRole('button', { name: 'Add connection' }));
    expect(lastBody('POST', API + '/connections')).toEqual(goConnectionCreate);
    expect(screen.queryByText('New connection', { selector: '.kv-card-title' })).toBeNull();
    expect(block.getByRole('button', { name: 'Add a connection' })).toBeInTheDocument();
  });

  // Guards the folded list: every stored connection starts folded to its label and one line (a danger badge when it
  // needs you, how many agents use it, since when), so the page does not show every field of every connection; a
  // card opens on its own, leaving the others folded; a card stays open after its own save, which remounts it; and a
  // connection being added is open at once. A list that opened every card, or snapped a card shut on save, fails this.
  it('folds each connection to a line and opens one at a time', async () => {
    await renderOrg(PATH, { routes: gitRoutes({ ['PUT ' + API + '/connections/' + LAB.id]: goIntegrationPageMultiple }) });
    for (const name of ['GitHub', 'Company GitLab']) {
      expect(card(name).tagName).toBe('DETAILS');
      expect(card(name)).not.toHaveAttribute('open');
    }
    const labLine = within(card('Company GitLab').querySelector('summary')!);
    expect(labLine.getByText('Reconnect needed')).toHaveClass('kv-badge--danger');
    expect(labLine.getByText('1 agent')).toBeInTheDocument();
    expect(labLine.getByText(/^Connected since /)).toBeInTheDocument();
    expect(within(card('GitHub').querySelector('summary')!).queryByText('Reconnect needed')).toBeNull();

    const lab = await openCard('Company GitLab');
    expect(card('Company GitLab')).toHaveAttribute('open');
    expect(card('GitHub')).not.toHaveAttribute('open');
    fireEvent.change(lab.getByLabelText('Access token'), { target: { value: 'glpat-new' } });
    await click(lab.getByRole('button', { name: 'Save connection' }));
    expect(card('Company GitLab')).toHaveAttribute('open');
    expect(card('GitHub')).not.toHaveAttribute('open');

    await click(screen.getByRole('button', { name: 'Add a connection' }));
    expect(card('New connection').tagName).toBe('SECTION');
    expect(within(card('New connection')).getByLabelText('Host')).toBeVisible();
  });

  // Guards the cards of an integration that takes several connections and has a connection test (connection_test
  // true; no built-in today, so the Git page stands in): each card shows its own last result, a failed one in a danger
  // banner, and tests its own id. A card that tested through the singular route, or showed another card's result,
  // would report on the wrong account.
  it('tests each card by its id when the integration has a connection test', async () => {
    const tested = {
      ...goIntegrationPageMultiple,
      connection_test: true,
      connections: [
        { ...HUB, verified_at: '2026-10-21T10:00:00Z', verified: true, verify_message: 'Connected as octo' },
        { ...LAB, verified_at: '2026-10-21T10:00:00Z', verified: false, verify_message: 'The host refused the token.' },
      ],
    };
    await renderOrg(PATH, { routes: gitRoutes({ ['GET ' + API]: tested, ['POST ' + API + '/connections/' + LAB.id + '/verify']: tested }) });
    expect(within(card('Company GitLab').querySelector('summary')!).getByText('Could not verify')).toHaveClass('kv-badge--danger');
    expect(within(card('GitHub').querySelector('summary')!).queryByText('Could not verify')).toBeNull();
    const hub = await openCard('GitHub');
    const lab = await openCard('Company GitLab');
    expect(hub.getByText(/^Verified .*: Connected as octo$/)).toBeInTheDocument();
    expect(lab.getByText('The host refused the token.').closest('[role="alert"]')).toHaveClass('kv-banner--danger');
    expect(lab.queryByText(/^Verified /)).toBeNull();
    await click(lab.getByRole('button', { name: 'Test connection' }));
    expect(called('POST', API + '/connections/' + LAB.id + '/verify')).toBe(true);
    expect(called('POST', API + '/connections/' + HUB.id + '/verify')).toBe(false);
  });

  // Guards Delete on one of several connections: it waits for the danger dialog, which names the agents whose access
  // goes with it (Lee's row names the GitLab connection, Dana's does not), then deletes that id alone. Deleting by the
  // singular route would take every connection and every agent's access.
  it('asks before deleting a connection, naming whose access goes with it', async () => {
    const after = { ...goIntegrationPageMultiple, connections: [HUB], access: [goIntegrationPageMultiple.access[0]!] };
    await renderOrg(PATH, { routes: gitRoutes({ ['DELETE ' + API + '/connections/' + LAB.id]: after }) });
    await click((await openCard('Company GitLab')).getByRole('button', { name: 'Delete' }));
    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveClass('kv-dialog--danger');
    expect(within(dialog).getByText('Delete Company GitLab?')).toBeInTheDocument();
    expect(within(dialog).getByText('Its stored secrets are deleted. Lee loses its access, which uses it.')).toBeInTheDocument();
    expect(called('DELETE', API + '/connections/' + LAB.id)).toBe(false);
    await click(within(dialog).getByRole('button', { name: 'Delete' }));
    expect(called('DELETE', API + '/connections/' + LAB.id)).toBe(true);
    expect(called('DELETE', API + '/connection')).toBe(false);
    expect(screen.queryByText('Company GitLab', { selector: '.kv-card-title' })).toBeNull();
  });

  // Guards the access dialog with several connections: a required Connection select comes first (Give access waits for
  // it), the row's connection is chosen when changing it, and the body names the chosen id (access_write.json); the access table says
  // which connection each agent uses. Without the select an agent would be set for a connection nobody chose.
  it('asks which connection an agent uses, and shows it on each row', async () => {
    await renderOrg(PATH, {
      routes: gitRoutes({ ['PUT ' + API + '/access/chief-of-staff']: goIntegrationPageMultiple, ['PUT ' + API + '/access/dana']: goIntegrationPageMultiple }),
    });
    const access = within(screen.getByRole('region', { name: 'Access' }));
    const table = within(access.getByRole('table'));
    expect(within(table.getByText('Dana').closest('tr') as HTMLElement).getByText('GitHub')).toBeInTheDocument();
    expect(within(table.getByText('Lee').closest('tr') as HTMLElement).getByText('Company GitLab')).toBeInTheDocument();

    await click(access.getByRole('button', { name: 'Give access' }));
    let dialog = within(screen.getByRole('dialog'));
    const select = dialog.getByRole('combobox', { name: 'Connection' });
    expect(select).toHaveValue('');
    expect(within(select).getAllByRole('option').map((o) => o.textContent)).toEqual(['Choose a connection', 'GitHub', 'Company GitLab']);
    expect(dialog.getByRole('button', { name: 'Give access' })).toBeDisabled();
    fireEvent.change(select, { target: { value: LAB.id } });
    await flush();
    fireEvent.change(dialog.getByRole('textbox', { name: 'Repositories' }), { target: { value: 'eng/*' } });
    await click(dialog.getByRole('button', { name: 'Add' }));
    await click(dialog.getByRole('button', { name: 'Give access' }));
    expect(lastBody('PUT', API + '/access/chief-of-staff')).toEqual(goAccessWrite);

    await click(screen.getByRole('button', { name: 'Change Dana’s access' }));
    dialog = within(screen.getByRole('dialog'));
    expect(dialog.getByRole('combobox', { name: 'Connection' })).toHaveValue(HUB.id);
    await click(dialog.getByRole('button', { name: 'Save access' }));
    expect(lastBody('PUT', API + '/access/dana')).toEqual({ access: { repositories: ['acme/*'], push: false }, connection_id: HUB.id });
  });

  // Guards the cards against each other: saving one starts that card again from what is stored, and leaves what was
  // typed into another card in place. A page-wide reset would throw away a token half typed into the other card.
  it('keeps what was typed into one card when another saves', async () => {
    await renderOrg(PATH, { routes: gitRoutes({ ['PUT ' + API + '/connections/' + LAB.id]: goIntegrationPageMultiple }) });
    fireEvent.change((await openCard('GitHub')).getByLabelText('Access token'), { target: { value: 'ghp-half-typed' } });
    const lab = await openCard('Company GitLab');
    fireEvent.change(lab.getByLabelText('Access token'), { target: { value: 'glpat-new' } });
    await click(lab.getByRole('button', { name: 'Save connection' }));
    expect(within(card('GitHub')).getByLabelText('Access token')).toHaveValue('ghp-half-typed');
    expect(within(card('Company GitLab')).getByLabelText('Access token')).toHaveValue('');
  });

  // Guards options listed through a connection: Load options waits until a connection is chosen, asks through the
  // chosen one, and choosing another connection drops the options and the values picked from them, which came through
  // the first. Otherwise an agent could be given a GitHub repository on its GitLab connection.
  it('lists options through the chosen connection, and drops them when it changes', async () => {
    const listed = {
      ...goIntegrationPageMultiple,
      access_form: {
        fields: [{ key: 'repos', title: 'Repositories', description: '', kind: 'choices' as const, required: false, options: [], choices: true, danger: false }],
      },
    };
    await renderOrg(PATH, {
      routes: gitRoutes({
        ['GET ' + API]: listed,
        ['GET ' + API + '/choices/repos?connection=' + HUB.id]: { options: ['acme/widgets'] },
        ['GET ' + API + '/choices/repos?connection=' + LAB.id]: { options: ['eng/tools'] },
        ['PUT ' + API + '/access/chief-of-staff']: listed,
      }),
    });
    await click(within(screen.getByRole('region', { name: 'Access' })).getByRole('button', { name: 'Give access' }));
    const dialog = within(screen.getByRole('dialog'));
    expect(dialog.getByRole('button', { name: 'Load options' })).toBeDisabled();
    expect(dialog.getByText('Choose a connection first: these are listed through it.')).toBeInTheDocument();
    fireEvent.change(dialog.getByRole('combobox', { name: 'Connection' }), { target: { value: HUB.id } });
    await flush();
    await click(dialog.getByRole('button', { name: 'Load options' }));
    await click(dialog.getByRole('checkbox', { name: 'acme/widgets' }));
    fireEvent.change(dialog.getByRole('combobox', { name: 'Connection' }), { target: { value: LAB.id } });
    await flush();
    expect(dialog.queryByRole('checkbox', { name: 'acme/widgets' })).toBeNull();
    await click(dialog.getByRole('button', { name: 'Load options' }));
    await click(dialog.getByRole('checkbox', { name: 'eng/tools' }));
    await click(dialog.getByRole('button', { name: 'Give access' }));
    expect(lastBody('PUT', API + '/access/chief-of-staff')).toEqual({ access: { repos: ['eng/tools'] }, connection_id: LAB.id });
  });

  // Guards the one-connection page against the several-connection one: with one connection (integration_page.json)
  // there is no card, no Add a connection, no Connection column and no Connection select.
  it('draws the one-connection page with no connection choice', async () => {
    await renderOrg('/org/integrations/hubspot');
    expect(screen.getByRole('region', { name: 'Connection' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Add a connection' })).toBeNull();
    expect(within(within(screen.getByRole('region', { name: 'Access' })).getByRole('table')).queryByText('Connection')).toBeNull();
    await click(screen.getByRole('button', { name: 'Give access' }));
    expect(within(screen.getByRole('dialog')).queryByRole('combobox', { name: 'Connection' })).toBeNull();
  });
});

describe('Integration page, a connection made by signing in', () => {
  const PATH = '/org/integrations/github';
  const API = '/api/v1/org/integrations/github';
  const routes = (page: typeof goIntegrationPageSignIn) => ({
    ['GET ' + API]: page,
    ['GET ' + API + '/audit']: { rows: [] },
    ['GET ' + API + '/logs']: { lines: [] },
  });

  // Guards the page of a sign-in connection (integration_page_sign_in.json): the manifest's setup notes render as
  // Markdown above the form, the form holds the client id and secret core adds (the secret only as Stored), and the
  // line under it says when the owner signed in and when the token is refreshed, both from the server's times. A page
  // that dropped the notes would leave the owner without the vendor-side steps; one that showed no refresh time would
  // hide when the connection next depends on the vendor.
  it('shows the setup notes, the client fields and when the token is refreshed', async () => {
    await renderOrg(PATH, { routes: routes(goIntegrationPageSignIn) });
    const block = within(screen.getByRole('region', { name: 'Connection' }));
    expect(block.getByText('Settings, Developer settings', { selector: 'strong' })).toBeInTheDocument();
    expect(block.getByLabelText('Client id')).toHaveValue('Iv23liAbCdEf');
    expect(block.getByLabelText('Client secret')).toHaveAttribute('placeholder', 'Stored');
    const c = goIntegrationPageSignIn.connection!;
    const now = Date.now();
    const want = 'Signed in ' + whenLabel(c.sign_in!.signed_in_at!, now) + ' · token refreshes ' + whenLabel(c.sign_in!.expires_at!, now);
    expect(block.getByText(want)).toBeInTheDocument();
    expect(block.queryByText(/^Needs you to sign in again/)).toBeNull();
  });

  // Guards the page of a setup connection (integration_page_setup.json): what the flow may do is in the Connection
  // block, so the owner sees where setup will send their browser and what it keeps on the page too, not only on the
  // install screen they approved weeks ago. A token page draws none of it.
  it('shows what a setup flow may do on the page', async () => {
    await renderOrg('/org/integrations/github', { routes: routes(goIntegrationPageSetup) });
    const block = within(screen.getByRole('region', { name: 'Connection' }));
    expect(block.getByText(goIntegrationPageSetup.setup!.summary)).toBeInTheDocument();
    expect(within(block.getByRole('list', { name: 'Sends your browser to' })).getByText('github.com')).toBeInTheDocument();
    expect(block.getByRole('list', { name: 'GitHub asks for' })).toBeInTheDocument();
    expect(block.getByRole('list', { name: 'Keeps' })).toBeInTheDocument();
    // A flow that hands over to a sign-in says so; and a setup connection has no form, so nothing to save or
    // disconnect sits under the declarations.
    expect(block.getByText('Then signs in through your browser, with the app that setup made.')).toBeInTheDocument();
    expect(block.queryByRole('button', { name: 'Save connection' })).toBeNull();
    expect(block.queryByRole('button', { name: 'Disconnect' })).toBeNull();
    cleanup();
    await renderOrg(PATH);
    expect(screen.queryByRole('list', { name: 'Keeps' })).toBeNull();
  });

  // Guards an address the setup notes give to copy rather than follow, such as GitHub's form for an organization,
  // which the owner opens after putting in a name: a code block in the notes is a CodeBlock headed by its info string,
  // not a link, and its Copy puts exactly the block's text on the clipboard, while the text around it stays Markdown.
  it('gives a code block in the setup notes its own Copy', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    const address = 'https://github.com/organizations/ORGANIZATION/settings/apps/new?public=false';
    const page = { ...goIntegrationPageSignIn, setup_notes_md: 'Using an **organization**? Copy this:\n\n```Organization address\n' + address + '\n```\n\nThen save.' };
    await renderOrg(PATH, { routes: routes(page) });
    const block = within(screen.getByRole('region', { name: 'Connection' }));
    expect(block.getByText('organization', { selector: 'strong' })).toBeInTheDocument();
    expect(block.getByText('Then save.')).toBeInTheDocument();
    const code = block.getByText('Organization address').closest('.kv-code') as HTMLElement;
    expect(within(code).getByText(address)).toBeInTheDocument();
    expect(within(code).queryByRole('link')).toBeNull();
    await click(within(code).getByRole('button', { name: 'Copy' }));
    expect(writeText).toHaveBeenCalledWith(address);
  });

  // Guards the page after the vendor refused a refresh (integration_page_sign_in_again.json): the line says the owner
  // must sign in again and gives the vendor's reason, in the danger tone, where the refresh time was; a page that
  // still read "Signed in" would hide why agents' calls fail.
  it('says the connection needs a new sign-in, and why', async () => {
    await renderOrg(PATH, { routes: routes(goIntegrationPageSignInAgain) });
    const line = screen.getByText('Needs you to sign in again: ' + goIntegrationPageSignInAgain.connection!.sign_in!.error!);
    expect(line).toBeInTheDocument();
    expect(screen.queryByText(/^Signed in /)).toBeNull();
  });

  // Guards the words for each state signInLine draws, including a connection never signed in and a token connection,
  // which has no line at all.
  it('words each sign-in state', () => {
    const now = Date.parse('2026-10-22T09:00:00Z');
    const c = goIntegrationPageSignIn.connection!;
    expect(signInLine(goIntegrationPage.connection, now)).toBeUndefined();
    expect(signInLine({ ...c, sign_in: { ways: ['device'], signed_in: false } }, now)).toEqual({ text: 'Not signed in yet.', danger: false });
    const { expires_at: _expires, ...noExpiry } = c.sign_in!;
    expect(signInLine({ ...c, sign_in: noExpiry }, now)?.text).toBe('Signed in ' + whenLabel(c.sign_in!.signed_in_at!, now));
    expect(signInLine(goIntegrationPageSignInAgain.connection, now)?.danger).toBe(true);
  });
});

describe('Integration page, setup flows', () => {
  const PATH = '/org/integrations/github';
  const API = '/api/v1/org/integrations/github';
  const signedIn = goIntegrationPageSignIn;
  const redirect = signedIn.sign_in_redirect_url!;
  // The same connection never signed in, its client id and secret stored, offered the ways given.
  const notSignedIn = (ways: string[]): typeof signedIn => {
    const sign_in = { ways, signed_in: false, ...(ways.includes('browser') ? { redirect_url: redirect } : {}) };
    return {
      ...signedIn,
      summary: { ...signedIn.summary, status: 'not_connected' },
      connection: { ...signedIn.connection!, sign_in },
      connections: [{ ...signedIn.connection!, sign_in }],
    };
  };
  const pageRoutes = (page: typeof signedIn, start: SetupFlow = goSetupFlowWait) => ({
    ['GET ' + API]: page,
    ['GET ' + API + '/audit']: { rows: [] },
    ['GET ' + API + '/logs']: { lines: [] },
    ['GET ' + API + '/setup']: fail(404, 'Kivali has no setup under way for this integration', 'you'),
    ['POST ' + API + '/setup']: start,
    ['POST ' + API + '/setup/cancel']: {},
  });
  const block = () => within(screen.getByRole('region', { name: 'Sign in' }));
  const reads = () => calls.filter((c) => c.method === 'GET' && c.url === API + '/setup').length;
  const tick = async () => {
    act(() => void vi.advanceTimersByTime(SETUP_POLL_MS));
    await flush();
  };

  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] });
    // The fixtures' flows were last changed at 10:00 and are kept until 10:10.
    vi.setSystemTime(new Date('2026-10-21T10:00:00Z'));
  });
  afterEach(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
  });

  // Guards a device sign-in drawn from its flow (setup_flow_wait.json): the one button the server's first way names
  // starts the flow (POST …/setup with the flow's name), and its Wait shows the code with Copy, the service's page as a
  // link that opens in a new tab without handing it this page (noopener), and how long the code works, from the
  // flow's expiry. The page asks how the flow stands every few seconds where it has no stream, and once the flow is
  // done it reads the page again, which says Signed in and offers Sign out. A page that never asked again would leave
  // the owner looking at a code long after they approved it.
  it('starts the sign-in, shows the code, and reads the page again once done', async () => {
    await renderOrg(PATH, { routes: pageRoutes(notSignedIn(['device'])) });
    expect(block().getByText('Signs in at github.com')).toBeInTheDocument();
    await click(block().getByRole('button', { name: 'Sign in with a code' }));
    expect(lastBody('POST', API + '/setup')).toEqual({ flow: 'setup' });
    expect(block().getByLabelText('Code')).toHaveTextContent('WDJB-MJHT');
    expect(block().getByRole('button', { name: 'Copy code' })).toBeInTheDocument();
    const link = block().getByRole('link', { name: 'github.com' });
    expect(link).toHaveAttribute('href', goSetupFlowWait.step!.url);
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener');
    expect(block().getByText('The code works for 10 minutes. Waiting.')).toBeInTheDocument();

    const before = reads();
    routes['GET ' + API + '/setup'] = goSetupFlowWait;
    await tick();
    expect(reads()).toBe(before + 1);
    expect(block().getByLabelText('Code')).toBeInTheDocument();

    routes['GET ' + API] = { ...signedIn, connection: { ...signedIn.connection!, verified: true, verify_message: 'Signed in as octocat' } };
    routes['GET ' + API + '/setup'] = goSetupFlowDone;
    await tick();
    expect(block().getByText('Signed in. Signed in as octocat')).toBeInTheDocument();
    expect(block().getByRole('button', { name: 'Sign out' })).toBeInTheDocument();
    expect(called('GET', API + '/audit')).toBe(true);
  });

  // Guards the wait while the tab is hidden: the page asks nothing then, and asks again once the tab shows. A page that
  // kept asking from a background tab would load the server for nobody, and keep a Wait checked for nobody.
  it('asks nothing while the tab is hidden', async () => {
    await renderOrg(PATH, { routes: pageRoutes(notSignedIn(['device'])) });
    await click(block().getByRole('button', { name: 'Sign in with a code' }));
    const before = reads();
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    act(() => void document.dispatchEvent(new Event('visibilitychange')));
    await tick();
    await tick();
    expect(reads()).toBe(before);
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    act(() => void document.dispatchEvent(new Event('visibilitychange')));
    await tick();
    expect(reads()).toBe(before + 1);
  });

  // Guards a flow's Form (setup_flow_form.json): the declared fields under the integration's name, a secret one as a
  // password field and one used during setup said to be never kept, and Continue posting what was typed to
  // …/setup/answer. Cancel ends the flow and the panel offers to start again. A Form drawn as text would show a key on
  // screen; one that posted elsewhere would never reach the flow.
  it('draws a form step and answers it', async () => {
    await renderOrg(PATH, { routes: pageRoutes(notSignedIn(['browser']), goSetupFlowForm) });
    await click(block().getByRole('button', { name: 'Sign in' }));
    expect(block().getByText('Name the account the app goes on.')).toBeInTheDocument();
    expect(block().getByText('GitHub asks for')).toBeInTheDocument();
    const key = block().getByLabelText('Key');
    expect(key).toHaveAttribute('type', 'password');
    expect(block().getByText('From the key page Used during setup and never kept.')).toBeInTheDocument();
    expect(block().getByLabelText('Account')).toHaveAttribute('type', 'text');
    fireEvent.change(block().getByLabelText('Account'), { target: { value: 'acme' } });
    fireEvent.change(key, { target: { value: 'pak-123' } });
    routes['POST ' + API + '/setup/answer'] = goSetupFlowRunning;
    await click(block().getByRole('button', { name: 'Continue' }));
    expect(lastBody('POST', API + '/setup/answer')).toEqual({ fields: { account: 'acme', personal_access_key: 'pak-123' } });
    await click(block().getByRole('button', { name: 'Cancel' }));
    expect(called('POST', API + '/setup/cancel')).toBe(true);
    expect(block().getByRole('button', { name: 'Sign in' })).toBeInTheDocument();
  });

  // Guards a running step (setup_flow_running.json): a real progress bar where the integration gave a count, named by
  // its words with the count for a screen reader, and Cancel; a step with no count shows its words alone, with no
  // bar that would claim a measure there is not.
  it('shows a running step’s progress as a bar where there is a count', async () => {
    await renderOrg(PATH, { routes: pageRoutes(notSignedIn(['browser']), goSetupFlowRunning) });
    await click(block().getByRole('button', { name: 'Sign in' }));
    const bar = block().getByRole('progressbar', { name: 'Building the app' });
    expect(bar).toHaveAttribute('aria-valuenow', '3');
    expect(bar).toHaveAttribute('aria-valuemax', '5');
    expect(bar).toHaveAttribute('aria-valuetext', 'Building the app, 3 of 5');
    expect(block().getByRole('button', { name: 'Cancel' })).toBeInTheDocument();
    routes['GET ' + API + '/setup'] = { ...goSetupFlowRunning, progress: { words: 'Asking GitHub', done: 0, total: 0 } };
    await tick();
    expect(block().queryByRole('progressbar')).toBeNull();
    expect(block().getByText('Asking GitHub')).toBeInTheDocument();
    expect(progressWords({ words: 'Asking GitHub', done: 0, total: 0 })).toBe('Asking GitHub');
  });

  // Guards a Go step (setup_flow_go.json): its button names the host it sends the browser to and sends this tab to the
  // step's start_url, /connect/start, which Kivali Desktop takes over to open the system browser, signed in. Sent
  // anywhere else (the service's page itself), a Desktop sign-in would open a browser with no Kivali session and no
  // flow cookie, and fail at the callback. Where the tab stays (Desktop), a line says what to do if no browser opened.
  // A step the service sends the browser nowhere from (setup_flow_go_no_return.json) opens in a new tab, so this page
  // stays to follow the flow.
  it('sends the browser on a go step through Kivali', async () => {
    const assign = vi.fn();
    vi.stubGlobal('location', { href: 'http://localhost' + PATH, pathname: PATH, search: '', assign });
    const open = vi.fn();
    vi.stubGlobal('open', open);
    await renderOrg(PATH, { routes: pageRoutes(notSignedIn(['browser']), goSetupFlowGo) });
    await click(block().getByRole('button', { name: 'Sign in' }));
    expect(block().getByText(goSetupFlowGo.step!.words)).toBeInTheDocument();
    await click(block().getByRole('button', { name: 'Continue to github.com' }));
    expect(assign).toHaveBeenCalledWith('/connect/start?flow=' + goSetupFlowGo.id);
    expect(block().getByText('If no browser opened, open the team in your browser and continue from there.')).toBeInTheDocument();
    expect(open).not.toHaveBeenCalled();

    routes['GET ' + API + '/setup'] = goSetupFlowGoNoReturn;
    await tick();
    await click(block().getByRole('button', { name: 'Continue to github.com' }));
    expect(open).toHaveBeenCalledWith('/connect/start?flow=' + goSetupFlowGo.id, '_blank', 'noopener');
    expect(assign).toHaveBeenCalledTimes(1);
  });

  // Guards a flow that failed (setup_flow_failed.json): the panel says it did not finish in the service's words and
  // offers Try again, which starts the flow again. A panel that kept the last step up would have the owner act on a
  // flow nothing waits for.
  it('says why a flow failed and offers Try again', async () => {
    await renderOrg(PATH, { routes: pageRoutes(notSignedIn(['device'])) });
    await click(block().getByRole('button', { name: 'Sign in with a code' }));
    routes['GET ' + API + '/setup'] = goSetupFlowFailed;
    await tick();
    expect(block().getByText('Setup did not finish')).toBeInTheDocument();
    expect(block().getByText(goSetupFlowFailed.error!)).toBeInTheDocument();
    expect(block().queryByLabelText('Code')).toBeNull();
    const before = reads();
    await tick();
    expect(reads()).toBe(before);
    await click(block().getByRole('button', { name: 'Try again' }));
    expect(calls.filter((c) => c.method === 'POST' && c.url === API + '/setup')).toHaveLength(2);
  });

  // Guards the way back from the service: Kivali's callback lands here with ?setup=<flow id> and no words, and the
  // panel reads the flow from the server. When the server no longer has it (it took too long, or Kivali restarted) the
  // panel says so and offers to start again; the query is taken off the address, so a reload does not read it again,
  // and anything in the query but a flow id is read as nothing.
  it('reads the flow a callback came back with', async () => {
    const h = await renderOrg(PATH + '?setup=' + goSetupFlowGo.id, { routes: pageRoutes(notSignedIn(['browser'])) });
    expect(called('GET', API + '/setup')).toBe(true);
    expect(block().getByText('This setup is no longer under way')).toBeInTheDocument();
    expect(block().getByRole('button', { name: 'Sign in' })).toBeInTheDocument();
    expect(h.router.state.location.search).toBe('');
    cleanup();
    await renderOrg(PATH + '?setup=' + goSetupFlowGo.id, { routes: { ...pageRoutes(notSignedIn(['browser'])), ['GET ' + API + '/setup']: goSetupFlowRunning } });
    expect(block().getByRole('progressbar')).toBeInTheDocument();
    expect(setupReturnOf('?setup=' + goSetupFlowGo.id)).toBe(goSetupFlowGo.id);
    expect(setupReturnOf('?setup=' + encodeURIComponent('Sign in at evil.example'))).toBeNull();
    expect(setupReturnOf('')).toBeNull();
  });

  // Guards Sign out: it posts to the connection's route and the page it answers replaces this one, so the block offers
  // signing in again rather than still reading Signed in.
  it('signs out', async () => {
    await renderOrg(PATH, { routes: { ...pageRoutes(signedIn), ['POST ' + API + '/connection/sign-out']: notSignedIn(['device']) } });
    await click(block().getByRole('button', { name: 'Sign out' }));
    expect(called('POST', API + '/connection/sign-out')).toBe(true);
    expect(block().getByRole('button', { name: 'Sign in with a code' })).toBeInTheDocument();
  });

  // Guards the scopes on the page (integration_page_sign_in.json asks for repo and read:org): the line sits with the
  // Sign in block, so the owner sees what they are about to grant before they sign in.
  it('says what the sign-in asks the service for', async () => {
    await renderOrg(PATH, { routes: pageRoutes(notSignedIn(['device'])) });
    const connection = within(screen.getByRole('region', { name: 'Connection' }));
    expect(connection.getByText('Asks the service for: repo, read:org')).toBeInTheDocument();
  });

  // Guards a manifest that names only ways this version does not sign in by: the block says so in one sentence
  // rather than show a button the server would refuse.
  it('says when this version of Kivali has no way to sign in', async () => {
    const none = { ...signedIn.connection!, sign_in: { ways: [], signed_in: false } };
    await renderOrg(PATH, { routes: pageRoutes({ ...signedIn, connection: none, connections: [none] }) });
    expect(block().getByText('This version of Kivali has no way to sign in to this service.')).toBeInTheDocument();
    expect(block().queryByRole('button')).toBeNull();
  });

  // Guards a connection that signs in only in the browser, read where that cannot finish
  // (integration_page_sign_in_elsewhere.json): no button at all, which would start a flow that cannot end, but words
  // that say to sign in from a browser and the team's address to open there, with Copy, since a link would only open
  // in this window. Without an address the words still say where.
  it('says to sign in from a browser at the team’s address where no way works', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    const page = goIntegrationPageSignInElsewhere;
    const openAt = page.connection!.sign_in!.open_at!;
    await renderOrg(PATH, { routes: pageRoutes(page) });
    expect(block().getByText('Signing in does not work from here. Open the team’s address in your browser and sign in there.')).toBeInTheDocument();
    expect(block().queryByRole('button', { name: /^Sign in/ })).toBeNull();
    expect(screen.queryByRole('textbox', { name: 'Redirect address' })).toBeNull();
    expect(block().getByRole('textbox', { name: 'Team address' })).toHaveValue(openAt);
    await click(block().getByRole('button', { name: 'Copy' }));
    expect(writeText).toHaveBeenCalledWith(openAt);
    cleanup();
    const { open_at: _openAt, ...noAddress } = page.connection!.sign_in!;
    const without = { ...page, connection: { ...page.connection!, sign_in: noAddress }, connections: [{ ...page.connection!, sign_in: noAddress }] };
    await renderOrg(PATH, { routes: pageRoutes(without) });
    expect(block().getByText('Signing in does not work from here. Open the team in your browser, at its own address, and sign in there.')).toBeInTheDocument();
    expect(block().queryByRole('textbox', { name: 'Team address' })).toBeNull();
  });

  // Guards the address the owner registers with the service (sign_in_redirect_url in integration_page_sign_in.json):
  // above the form, read-only so it cannot be changed by a slip before it is copied, with Copy, and the sentence that
  // says what to do with it. A wrong address there is a service that refuses every sign-in.
  it('shows the redirect address to copy', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    await renderOrg(PATH, { routes: pageRoutes(notSignedIn(['browser'])) });
    const field = screen.getByRole('textbox', { name: 'Redirect address' });
    expect(field).toHaveValue(redirect);
    expect(field).toHaveAttribute('readonly');
    expect(screen.getByText('Register this address as the app’s callback URL.')).toBeInTheDocument();
    await click(screen.getByRole('button', { name: 'Copy' }));
    expect(writeText).toHaveBeenCalledWith(redirect);
    expect(screen.getByRole('button', { name: 'Copied' })).toBeInTheDocument();
  });

  // Guards a flow Kivali could not tie to any integration (it restarted, or the address was pasted): the callback
  // lands on the integrations list with start_again, which must say what happened rather than show the list as if
  // nothing did; words in the address are never shown there.
  it('shows a sign-in error on the integrations list', async () => {
    const h = await renderOrg('/org/integrations?sign_in_error=start_again');
    expect(screen.getByText('The sign-in did not work')).toBeInTheDocument();
    expect(screen.getByText('The sign-in took too long or was started elsewhere. Start it again.')).toBeInTheDocument();
    expect(h.router.state.location.search).toBe('');
    cleanup();
    await renderOrg('/org/integrations?sign_in_error=' + encodeURIComponent('Call 555-0100 to restore access'));
    expect(screen.queryByText('The sign-in did not work')).toBeNull();
    expect(screen.queryByText(/555-0100/)).toBeNull();
  });

  // Guards where the owner is sent (sign_in_at in integration_page_sign_in.json): the words for each way's host, once.
  it('names the hosts the sign-in goes to', () => {
    expect(signsInAt(['browser', 'device'], { browser: 'app.hubspot.com', device: 'login.example.com' })).toBe(
      'Signs in at app.hubspot.com and login.example.com',
    );
    expect(signsInAt(['browser'], undefined)).toBeUndefined();
  });

  // Guards the install screen of a sign-in bundle (integration_version_sign_in.json): before approving, the owner sees
  // every host the sign-in talks to and that their browser is sent there, so a lookalike endpoint is in plain sight
  // rather than only in the manifest. A token bundle's screen has no such block.
  it('lists the sign-in hosts on the install screen', async () => {
    await renderOrg('/org/integrations/hubspot/versions/1.3.0', {
      routes: { 'GET /api/v1/org/integrations/hubspot/versions/1.3.0': goIntegrationVersionSignIn },
    });
    const hosts = within(screen.getByRole('region', { name: 'Signs in at' }));
    for (const h of goIntegrationVersionSignIn.sign_in_hosts) expect(hosts.getByText(h)).toBeInTheDocument();
    expect(screen.getByText(/sends your browser to the service’s sign-in page here/)).toBeInTheDocument();
    cleanup();
    await renderOrg('/org/integrations/hubspot/versions/1.3.0');
    expect(screen.queryByText('Signs in at')).toBeNull();
  });

  // Guards the words of a code's life, rounded to whole minutes and never under one.
  it('words how long a code works', () => {
    const now = Date.parse('2026-10-21T10:00:00Z');
    expect(codeLifeWords('2026-10-21T10:10:00Z', now)).toBe('The code works for 10 minutes.');
    expect(codeLifeWords('2026-10-21T10:00:20Z', now)).toBe('The code works for 1 minute.');
  });

  // Guards every connection being made by a flow: a token integration with no connection yet offers Connect, whose
  // flow asks for the connection's form, rather than a form saved by PUT; a setup integration's page offers Set up in
  // its place. Each starts its flow with POST …/setup.
  it('connects a token or setup integration through its flow', async () => {
    const hubspot = '/api/v1/org/integrations/hubspot';
    const unconnected = { ...goIntegrationPage, connection: undefined, connections: [] } as unknown as IntegrationPageData;
    await renderOrg('/org/integrations/hubspot', {
      routes: {
        ['GET ' + hubspot]: unconnected,
        ['GET ' + hubspot + '/setup']: fail(404, 'none', 'you'),
        ['POST ' + hubspot + '/setup']: goSetupFlowForm,
      },
    });
    const connect = within(screen.getByRole('region', { name: 'Connect' }));
    expect(screen.queryByRole('button', { name: 'Save connection' })).toBeNull();
    await click(connect.getByRole('button', { name: 'Connect' }));
    expect(lastBody('POST', hubspot + '/setup')).toEqual({ flow: 'setup' });
    expect(connect.getByLabelText('Account')).toBeInTheDocument();
    cleanup();
    await renderOrg(PATH, { routes: { ...pageRoutes(goIntegrationPageSetup), ['POST ' + API + '/setup']: goSetupFlowGo } });
    const setup = within(screen.getByRole('region', { name: 'Set up' }));
    await click(setup.getByRole('button', { name: 'Set up' }));
    expect(setup.getByRole('button', { name: 'Continue to github.com' })).toBeInTheDocument();
  });
});

describe('connection words', () => {
  // Guards the delete dialog's sentence for every count of agents: none, one (its) and several (their, with "and").
  it('names the agents whose access a delete takes', () => {
    expect(deleteConnectionWords([])).toBe('Its stored secrets are deleted. No agent’s access uses it.');
    expect(deleteConnectionWords(['Dana'])).toBe('Its stored secrets are deleted. Dana loses its access, which uses it.');
    expect(deleteConnectionWords(['Dana', 'Lee', 'Kim'])).toBe('Its stored secrets are deleted. Dana, Lee and Kim lose their access, which uses it.');
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
