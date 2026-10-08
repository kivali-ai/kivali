import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, apiGet, apiPost, apiPostForm } from '../../api/client';
import type { AutoRelease, HistoryResponse, Home as HomeData } from '../../api/types.gen';
import { AgentAvatar, TooltipProvider } from '../../ds';
import { identityColorFor } from '../../lib/agentIdentity';
import { pickFiles } from '../../lib/pickFiles';
import { goSnapshot } from '../../state/fixtures';
import { busyHome, emptyHome, goHistory, goNeedAction, quietHome } from '../../state/fixtures/home';
import { initialOrgState, reduceSnapshot } from '../../state/org';
import type { OrgState } from '../../state/org';
import type { OrgContextValue } from '../../state/OrgProvider';
import { Home } from './Home';

// ---- Seams: the API client, the org provider (a tiny store the test drives) and the file picker ----

vi.mock('../../api/client', async (importOriginal) => {
  const real = await importOriginal<typeof import('../../api/client')>();
  return { ...real, apiGet: vi.fn(), apiPost: vi.fn(), apiPostForm: vi.fn() };
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

const getMock = vi.mocked(apiGet);
const postMock = vi.mocked(apiPost);
const formMock = vi.mocked(apiPostForm);
const setAutoRelease = vi.fn(async (_v: AutoRelease) => {});

function orgFor(home: HomeData, over: Partial<OrgState> = {}): OrgState {
  return { ...reduceSnapshot(initialOrgState, goSnapshot), goals: home.goals, readouts: home.readouts, autoRelease: home.auto_release, ...over };
}

function setOrg(org: OrgState) {
  const value: Partial<OrgContextValue> = { org, setAutoRelease, me: null };
  act(() => orgStore.set(value));
}

function currentOrg(): OrgState {
  return (orgStore.get() as OrgContextValue).org;
}

function serve(home: HomeData, history: Record<string, HistoryResponse> = {}) {
  getMock.mockImplementation(async (path: string) => {
    if (path === '/api/v1/home') return structuredClone(home);
    const page = history[path];
    if (page) return structuredClone(page);
    throw new ApiError(404, 'not found', 'Nobody');
  });
}

async function flush() {
  await act(async () => {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  });
}

function desktop(on: boolean) {
  window.matchMedia = ((query: string) =>
    ({
      matches: on && query.includes('min-width'),
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }) as unknown as MediaQueryList) as typeof window.matchMedia;
}

async function renderHome(home: HomeData, opts: { path?: string; org?: Partial<OrgState>; history?: Record<string, HistoryResponse> } = {}) {
  serve(home, opts.history);
  setOrg(orgFor(home, opts.org));
  const router = createMemoryRouter(
    [
      { path: '/', element: <Home /> },
      { path: '/agents/:slug', element: <p>agent chat</p> },
      { path: '/assignments/:id', element: <p>assignment</p> },
    ],
    { basename: '/', initialEntries: [(opts.path ?? '/')] },
  );
  const utils = render(
    <TooltipProvider>
      <RouterProvider router={router} />
    </TooltipProvider>,
  );
  await flush();
  return { ...utils, router };
}

function rowFor(attr: 'data-id' | 'data-path', value: string): HTMLElement {
  const el = document.querySelector<HTMLElement>(`[${attr}="${value}"]`);
  if (!el) throw new Error('no row ' + value);
  return el;
}

const [approval, proposal, notification, assignment, needsHelp] = busyHome.needs as [
  (typeof busyHome.needs)[number],
  (typeof busyHome.needs)[number],
  (typeof busyHome.needs)[number],
  (typeof busyHome.needs)[number],
  (typeof busyHome.needs)[number],
];
const [noticeRow, heldAssignment, secondNotice] = busyHome.queue as [
  (typeof busyHome.queue)[number],
  (typeof busyHome.queue)[number],
  (typeof busyHome.queue)[number],
];

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] });
  vi.setSystemTime(new Date('2026-09-20T17:00:00Z'));
  desktop(false);
  getMock.mockReset();
  postMock.mockReset().mockResolvedValue({});
  formMock.mockReset().mockResolvedValue({});
  setAutoRelease.mockClear();
  vi.mocked(pickFiles).mockReset();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('Home renders', () => {
  it('draws a busy morning: in flight, needs you and the queue', async () => {
    await renderHome(busyHome);
    expect(screen.getByRole('heading', { name: 'In flight' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '2 agents working' })).toHaveAttribute('href', '/team');
    expect(screen.getByRole('link', { name: '$4.25 today' })).toHaveAttribute('href', '/org');
    expect(screen.getByRole('link', { name: 'Ship the release' })).toHaveAttribute('href', '/assignments/40');
    expect(screen.getByText('7 of 12')).toBeInTheDocument();
    expect(screen.getByText('Waiting on #45, Run the tests')).toBeInTheDocument();

    const needs = screen.getByRole('region', { name: 'Needs you' });
    expect(within(needs).getByLabelText('5 waiting on you')).toHaveTextContent('5');
    for (const kind of ['Approval', 'Hire', 'Notice', 'Assigned to you', 'Needs help']) expect(within(needs).getByText(kind)).toBeInTheDocument();

    const queue = screen.getByRole('region', { name: 'Queue' });
    expect(within(queue).getByText('3 passing through')).toBeInTheDocument();
    // Top to bottom: Needs you, In flight, Queue.
    const inFlight = screen.getByRole('region', { name: 'In flight' });
    expect(needs.compareDocumentPosition(inFlight) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(inFlight.compareDocumentPosition(queue) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(queue).getByRole('button', { name: 'Release all' })).toBeInTheDocument();
    // Phone: the Select variant and the History link at the foot, no tabs.
    expect(within(queue).getByRole('combobox', { name: 'Auto-release' })).toHaveValue('30s');
    expect(screen.getByRole('link', { name: 'History' })).toHaveAttribute('href', '/?view=history');
    expect(screen.queryByRole('tab')).toBeNull();
  });

  it('draws a quiet afternoon with countdowns and nothing waiting', async () => {
    await renderHome(quietHome);
    expect(screen.getByText('Nothing waiting on you')).toBeInTheDocument();
    expect(screen.getByText('nothing waiting')).toBeInTheDocument();
    expect(screen.getByText('Releases in 0:30')).toBeInTheDocument();
    expect(screen.getByText('Releases in 1:05')).toBeInTheDocument();
  });

  it('draws empty states and keeps the queue header', async () => {
    await renderHome(emptyHome);
    expect(screen.getByText('Nothing waiting on you')).toBeInTheDocument();
    expect(screen.getByText('No deliveries queued')).toBeInTheDocument();
    expect(screen.getByText('empty')).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Auto-release' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Release all' })).toBeNull();
  });

  it('uses the slider and the Now · History tabs on desktop', async () => {
    desktop(true);
    await renderHome(busyHome);
    expect(screen.getByRole('radiogroup', { name: 'Auto-release' })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'Now' })).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByRole('tab', { name: 'History' })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'History' })).toBeNull();
  });

  it('shows skeletons only after a slow load, and a banner when it fails', async () => {
    getMock.mockImplementation(() => new Promise(() => {}));
    setOrg(orgFor(busyHome));
    const router = createMemoryRouter([{ path: '/', element: <Home /> }], { basename: '/', initialEntries: ['/'] });
    render(<RouterProvider router={router} />);
    expect(screen.queryByLabelText('Loading Home')).toBeNull();
    act(() => void vi.advanceTimersByTime(300));
    expect(screen.getByLabelText('Loading Home')).toBeInTheDocument();
  });

  it('says what happened and who can fix it when Home cannot load', async () => {
    getMock.mockRejectedValue(new ApiError(503, 'no model is connected yet', 'Whoever runs this Kivali server.'));
    setOrg(orgFor(busyHome));
    const router = createMemoryRouter([{ path: '/', element: <Home /> }], { basename: '/', initialEntries: ['/'] });
    render(<RouterProvider router={router} />);
    await flush();
    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent('No model is connected yet.');
    expect(alert).toHaveTextContent('Whoever runs this Kivali server.');
  });
});

describe('Home follows the design rules', () => {
  function tileColor(el: Element): string {
    return (el as HTMLElement).style.background;
  }

  it('draws each agent with one identity colour everywhere and its role icon from its title', async () => {
    await renderHome(busyHome);
    const want = 'var(--identity-' + identityColorFor('engineering-lead') + ')';
    // Goal owner (24), the approval row (32) and the queue sender (24).
    const tiles = screen.getAllByRole('img', { name: 'Engineering lead' });
    expect(tiles.length).toBeGreaterThanOrEqual(3);
    for (const t of tiles) expect(tileColor(t)).toBe(want);

    // The 32px row tile carries the role icon the agent's record names (snapshot.json: code).
    const rowTile = within(rowFor('data-id', approval.id)).getByRole('img', { name: 'Engineering lead' });
    const { container } = render(<AgentAvatar name="Engineering lead" role="code" size={32} />);
    expect(rowTile.querySelector('.kv-avatar-role')?.innerHTML).toBe(container.querySelector('.kv-avatar-role')?.innerHTML);

    // An agent the tree does not list (archived) keeps its slug's colour.
    const tester = within(rowFor('data-id', needsHelp.id)).getByRole('img', { name: 'tester' });
    expect(tileColor(tester)).toBe('var(--identity-' + identityColorFor('tester') + ')');
  });

  it('keeps one signal highlight, the Needs-you count, and neutral kind badges', async () => {
    await renderHome(busyHome);
    const signals = document.querySelectorAll('.kv-badge--signal');
    expect(signals).toHaveLength(1);
    expect(signals[0]).toHaveAccessibleName('5 waiting on you');
    const needs = screen.getByRole('region', { name: 'Needs you' });
    for (const kind of ['Approval', 'Hire', 'Notice', 'Assigned to you', 'Needs help']) expect(within(needs).getByText(kind)).toHaveClass('kv-badge--neutral');
  });

  it('keeps Release all the one primary until a row opens, and Open chat secondary', async () => {
    await renderHome(busyHome);
    const primaries = () => [...document.querySelectorAll('.kv-btn--primary')].map((b) => b.textContent);
    expect(primaries()).toEqual(['Release all']);
    fireEvent.click(screen.getByRole('button', { name: /keeps restarting/ }));
    expect(within(rowFor('data-id', needsHelp.id)).getByRole('button', { name: 'Open chat' })).toHaveClass('kv-btn--secondary');
    expect(primaries()).toEqual(['Release all']);
  });

  it('says which rows are open', async () => {
    await renderHome(busyHome);
    const row = screen.getByRole('button', { name: /Approve \$1,840/ });
    expect(row).toHaveAttribute('aria-expanded', 'false');
    fireEvent.click(row);
    expect(row).toHaveAttribute('aria-expanded', 'true');
    const queueRow = screen.getByRole('button', { name: /Release update/ });
    expect(queueRow).toHaveAttribute('aria-expanded', 'false');
  });

  it('never makes a countdown a live region', async () => {
    await renderHome(quietHome);
    const queue = screen.getByRole('region', { name: 'Queue' });
    expect(within(queue).getByText('Releases in 0:30').closest('[aria-live], [role="status"], [role="timer"], [role="alert"]')).toBeNull();
  });

  it('ticks once a second only while a countdown runs', async () => {
    const clock = vi.fn(() => Date.now());
    serve({ ...busyHome, auto_release: 'off' });
    setOrg(orgFor({ ...busyHome, auto_release: 'off' }));
    const router = createMemoryRouter([{ path: '/', element: <Home clock={clock} /> }], { basename: '/', initialEntries: ['/'] });
    const { unmount } = render(<RouterProvider router={router} />);
    await flush();
    clock.mockClear();
    act(() => void vi.advanceTimersByTime(5000));
    expect(clock).not.toHaveBeenCalled();

    setOrg({ ...currentOrg(), autoRelease: '30s' });
    await flush();
    clock.mockClear();
    act(() => void vi.advanceTimersByTime(5000));
    expect(clock).toHaveBeenCalledTimes(5);
    unmount();
    expect(vi.getTimerCount()).toBe(0);
  });
});

describe('Needs you', () => {
  it('shows the action set for each kind when a row expands', async () => {
    await renderHome(busyHome);

    fireEvent.click(screen.getByRole('button', { name: /Approve \$1,840/ }));
    let row = within(rowFor('data-id', approval.id));
    expect(row.getByLabelText('Note (optional)')).toBeInTheDocument();
    expect(row.getByRole('button', { name: 'Deny' })).toBeInTheDocument();
    expect(row.getByRole('button', { name: 'Approve' })).toBeInTheDocument();
    expect(row.getByRole('button', { name: 'Attach' })).toBeInTheDocument();
    expect(row.getByRole('button', { name: 'hosting-quote.pdf · 84 KB' })).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /Weekly report/ }));
    row = within(rowFor('data-id', notification.id));
    expect(row.getByLabelText('Reply (optional)')).toBeInTheDocument();
    expect(row.getByRole('button', { name: 'Acknowledge' })).toBeInTheDocument();
    expect(row.queryByRole('button', { name: 'Approve' })).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: /assigned to you: Budget/ }));
    row = within(rowFor('data-id', assignment.id));
    expect(row.getByRole('combobox', { name: 'Resolution' })).toHaveValue('done');
    expect(row.getByLabelText('Outcome')).toBeInTheDocument();
    expect(row.getByRole('link', { name: 'Open #52' })).toHaveAttribute('href', '/assignments/52');
    expect(row.getByRole('button', { name: 'Hand back' })).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /keeps restarting/ }));
    row = within(rowFor('data-id', needsHelp.id));
    expect(row.getByRole('button', { name: 'Open chat' })).toBeInTheDocument();

    // A proposal opens its review page instead of expanding.
    const review = within(rowFor('data-id', proposal.id)).getByRole('link', { name: /Approve hire: Garden advisor/ });
    expect(review).toHaveAttribute('href', proposal.review_path);
    expect(review).toHaveTextContent('Review');
  });

  it('approves with the message and attachments as multipart, then the row leaves', async () => {
    const file = new File(['quote'], 'po.pdf', { type: 'application/pdf' });
    vi.mocked(pickFiles).mockResolvedValue([file]);
    formMock.mockResolvedValue(goNeedAction);
    await renderHome(busyHome);

    fireEvent.click(screen.getByRole('button', { name: /Approve \$1,840/ }));
    const row = within(rowFor('data-id', approval.id));
    fireEvent.change(row.getByLabelText('Note (optional)'), { target: { value: 'Go ahead.' } });
    fireEvent.click(row.getByRole('button', { name: 'Attach' }));
    await flush();
    expect(row.getByText('po.pdf')).toBeInTheDocument();
    fireEvent.click(row.getByRole('button', { name: 'Approve' }));
    await flush();

    expect(formMock).toHaveBeenCalledTimes(1);
    const [url, form] = formMock.mock.calls[0] as [string, FormData];
    expect(url).toBe('/api/v1/needs/approve');
    expect(form.get('path')).toBe(approval.id);
    expect(form.get('message')).toBe('Go ahead.');
    expect((form.getAll('attachments[]')[0] as File).name).toBe('po.pdf');
    expect(document.querySelector(`[data-id="${approval.id}"]`)).toBeNull();
    expect(screen.getByLabelText('4 waiting on you')).toBeInTheDocument();

    // The answer's result shows as a toast with a link, and leaves on its own.
    const toast = screen.getAllByRole('status').find((el) => el.textContent?.includes('Garden advisor is hired and reports to Chief of Staff'));
    if (!toast) throw new Error('no toast');
    expect(within(toast).getByRole('button', { name: 'Open' })).toBeInTheDocument();
    act(() => void vi.advanceTimersByTime(8000));
    expect(screen.queryByText('Garden advisor is hired and reports to Chief of Staff')).toBeNull();
  });

  it('denies and acknowledges through their own endpoints', async () => {
    await renderHome(busyHome);
    fireEvent.click(screen.getByRole('button', { name: /Approve \$1,840/ }));
    fireEvent.click(within(rowFor('data-id', approval.id)).getByRole('button', { name: 'Deny' }));
    await flush();
    expect(formMock.mock.calls[0]?.[0]).toBe('/api/v1/needs/deny');

    fireEvent.click(screen.getByRole('button', { name: /Weekly report/ }));
    fireEvent.click(within(rowFor('data-id', notification.id)).getByRole('button', { name: 'Acknowledge' }));
    await flush();
    const [url, form] = formMock.mock.calls[1] as [string, FormData];
    expect(url).toBe('/api/v1/needs/ack');
    expect(form.get('path')).toBe(notification.id);
    expect(document.querySelector(`[data-id="${notification.id}"]`)).toBeNull();
  });

  it('puts the row back and says why when the server refuses', async () => {
    formMock.mockRejectedValue(new ApiError(409, 'that request was already answered', 'You can refresh to see where it went.'));
    await renderHome(busyHome);
    fireEvent.click(screen.getByRole('button', { name: /Approve \$1,840/ }));
    fireEvent.click(within(rowFor('data-id', approval.id)).getByRole('button', { name: 'Approve' }));
    await flush();
    expect(rowFor('data-id', approval.id)).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent('That request was already answered.');
  });

  it('hands an assignment back with a resolution and an outcome', async () => {
    await renderHome(busyHome);
    fireEvent.click(screen.getByRole('button', { name: /assigned to you: Budget/ }));
    const row = within(rowFor('data-id', assignment.id));
    fireEvent.click(row.getByRole('button', { name: 'Hand back' }));
    expect(row.getByText('Say what was done, or why it is dropped.')).toBeInTheDocument();
    expect(postMock).not.toHaveBeenCalled();

    fireEvent.change(row.getByRole('combobox', { name: 'Resolution' }), { target: { value: 'dropped' } });
    fireEvent.change(row.getByLabelText('Outcome'), { target: { value: 'No longer needed.' } });
    fireEvent.click(row.getByRole('button', { name: 'Hand back' }));
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/assignments/52/close', { resolution: 'dropped', outcome: 'No longer needed.' });
    expect(document.querySelector(`[data-id="${assignment.id}"]`)).toBeNull();
  });

  it('opens the chat of an agent that needs help', async () => {
    const { router } = await renderHome(busyHome);
    fireEvent.click(screen.getByRole('button', { name: /keeps restarting/ }));
    fireEvent.click(within(rowFor('data-id', needsHelp.id)).getByRole('button', { name: 'Open chat' }));
    await flush();
    expect(router.state.location.pathname).toBe('/agents/tester');
  });
});

describe('Queue', () => {
  it('releases one row in one step and the row leaves', async () => {
    await renderHome(busyHome);
    fireEvent.click(within(rowFor('data-path', noticeRow.path)).getByRole('button', { name: 'Release' }));
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/queue/release', { path: noticeRow.path });
    expect(document.querySelector(`[data-path="${noticeRow.path}"]`)).toBeNull();
    expect(screen.getByText('2 passing through')).toBeInTheDocument();
  });

  it('brings a row back when its release fails', async () => {
    postMock.mockRejectedValue(new ApiError(0, 'Kivali could not reach the server.', 'Check your connection, then try again.'));
    await renderHome(busyHome);
    fireEvent.click(within(rowFor('data-path', noticeRow.path)).getByRole('button', { name: 'Release' }));
    await flush();
    expect(rowFor('data-path', noticeRow.path)).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent('Kivali could not reach the server.');
  });

  it('releases with a note from the expanded row', async () => {
    await renderHome(busyHome);
    fireEvent.click(screen.getByRole('button', { name: /Release update/ }));
    const row = within(rowFor('data-path', noticeRow.path));
    expect(row.getByRole('link', { name: 'Raw message' })).toHaveAttribute('href', noticeRow.raw_url);
    expect(row.queryByRole('button', { name: 'Release with note' })).toBeNull();
    fireEvent.change(row.getByLabelText('Note to the recipient (optional)'), { target: { value: 'Ship it.' } });
    fireEvent.click(row.getAllByRole('button', { name: 'Release' }).at(-1)!);
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/queue/release', { path: noticeRow.path, note: 'Ship it.' });
  });

  it('releases without a note from the expanded row when the note is blank', async () => {
    await renderHome(busyHome);
    fireEvent.click(screen.getByRole('button', { name: /Release update/ }));
    const row = within(rowFor('data-path', noticeRow.path));
    fireEvent.change(row.getByLabelText('Note to the recipient (optional)'), { target: { value: '   ' } });
    const release = row.getAllByRole('button', { name: 'Release' }).at(-1)!;
    expect(release).toBeEnabled();
    fireEvent.click(release);
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/queue/release', { path: noticeRow.path });
  });

  it('releases everything with Release all while auto-release runs; no checkboxes', async () => {
    await renderHome(busyHome);
    expect(screen.queryByRole('checkbox')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Release all' }));
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/queue/release-all', { paths: busyHome.queue.map((q) => q.path) });
    expect(screen.getByText('No deliveries queued')).toBeInTheDocument();
  });

  it('releases only the selected rows the filter shows', async () => {
    await renderHome({ ...busyHome, auto_release: 'off' });
    fireEvent.click(within(rowFor('data-path', noticeRow.path)).getByRole('checkbox', { name: 'Select message' }));
    fireEvent.click(within(rowFor('data-path', heldAssignment.path)).getByRole('checkbox', { name: 'Select message' }));
    expect(screen.getByRole('button', { name: 'Release 2 selected' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Notices' }));
    expect(screen.getByText('1 selected')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Release 1 selected' }));
    await flush();
    expect(postMock).toHaveBeenCalledTimes(1);
    expect(postMock).toHaveBeenCalledWith('/api/v1/queue/release-all', { paths: [noticeRow.path] });
    fireEvent.click(screen.getByRole('button', { name: 'All' }));
    expect(rowFor('data-path', heldAssignment.path)).toBeInTheDocument();
  });

  it('releases the selection when auto-release is Off', async () => {
    await renderHome({ ...busyHome, auto_release: 'off' });
    expect(screen.getByText('3 held')).toBeInTheDocument();
    fireEvent.click(within(rowFor('data-path', noticeRow.path)).getByRole('checkbox', { name: 'Select message' }));
    fireEvent.click(within(rowFor('data-path', secondNotice.path)).getByRole('checkbox', { name: 'Select message' }));
    expect(screen.getByText('2 selected')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Release 2 selected' }));
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/queue/release-all', { paths: [noticeRow.path, secondNotice.path] });
    expect(rowFor('data-path', heldAssignment.path)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Release all' })).toBeInTheDocument();
  });

  it('bounces a notice only with a note, with no dialog', async () => {
    await renderHome(busyHome);
    fireEvent.click(screen.getByRole('button', { name: /assigned to you: Run the tests/ }));
    expect(within(rowFor('data-path', heldAssignment.path)).queryByRole('button', { name: /Bounce/ })).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: /Release update/ }));
    const row = within(rowFor('data-path', noticeRow.path));
    const bounce = row.getByRole('button', { name: 'Bounce with note' });
    expect(bounce).toBeDisabled();
    fireEvent.change(row.getByLabelText('Note to the recipient (optional)'), { target: { value: '  Wait for the test results. ' } });
    expect(bounce).toBeEnabled();
    fireEvent.click(bounce);
    expect(screen.queryByRole('dialog')).toBeNull();
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/queue/bounce', { path: noticeRow.path, comment: 'Wait for the test results.' });
    expect(document.querySelector(`[data-path="${noticeRow.path}"]`)).toBeNull();
  });

  it('filters by assignments and notices', async () => {
    await renderHome(busyHome);
    fireEvent.click(screen.getByRole('button', { name: 'Assignments' }));
    expect(screen.getByRole('button', { name: 'Assignments' })).toHaveAttribute('aria-pressed', 'true');
    expect(document.querySelectorAll('[data-path]')).toHaveLength(1);
    expect(rowFor('data-path', heldAssignment.path)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Notices' }));
    expect(document.querySelectorAll('[data-path]')).toHaveLength(2);
    // Release all under a filter releases what the filter shows.
    fireEvent.click(screen.getByRole('button', { name: 'Release all' }));
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/queue/release-all', { paths: [noticeRow.path, secondNotice.path] });
  });

  it('counts down once a second, shows Held, and holds everything when auto-release turns Off', async () => {
    const { unmount } = await renderHome(busyHome);
    const notice = within(rowFor('data-path', noticeRow.path));
    expect(notice.getByText('Releases in 0:30')).toBeInTheDocument();
    expect(within(rowFor('data-path', heldAssignment.path)).getByText('Held')).toBeInTheDocument();
    act(() => void vi.advanceTimersByTime(1000));
    expect(notice.getByText('Releases in 0:29')).toBeInTheDocument();
    act(() => void vi.advanceTimersByTime(29_000));
    expect(notice.getByText('Releasing')).toBeInTheDocument();

    setOrg({ ...currentOrg(), autoRelease: 'off' });
    await flush();
    expect(within(rowFor('data-path', noticeRow.path)).getByText('Held')).toBeInTheDocument();
    unmount();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('writes the auto-release setting through the org provider', async () => {
    await renderHome(busyHome);
    fireEvent.change(screen.getByRole('combobox', { name: 'Auto-release' }), { target: { value: 'Off' } });
    expect(setAutoRelease).toHaveBeenCalledWith('off');
  });
});

describe('refetch', () => {
  it('refetches when the snapshot moves the queue, the inbox, messages or the tracker', async () => {
    await renderHome(busyHome);
    expect(getMock).toHaveBeenCalledTimes(1);

    setOrg({ ...currentOrg(), running: !currentOrg().running });
    await flush();
    expect(getMock).toHaveBeenCalledTimes(1);

    setOrg({ ...currentOrg(), pendingPaths: [] });
    await flush();
    expect(getMock).toHaveBeenCalledTimes(2);

    setOrg({ ...currentOrg(), assignmentsVersion: currentOrg().assignmentsVersion + 1 });
    await flush();
    expect(getMock).toHaveBeenCalledTimes(3);
  });

  it('shows new rows in place and resets countdowns after an auto-release change', async () => {
    await renderHome(busyHome);
    const moved: HomeData = {
      ...busyHome,
      auto_release: '2m',
      queue: busyHome.queue.map((q) => (q.path === noticeRow.path ? { ...q, releases_at: '2026-09-20T17:02:00Z' } : q)),
    };
    serve(moved);
    setOrg({ ...currentOrg(), autoRelease: '2m' });
    await flush();
    expect(getMock).toHaveBeenCalledTimes(2);
    expect(within(rowFor('data-path', noticeRow.path)).getByText('Releases in 2:00')).toBeInTheDocument();
    // The snapshot that follows the server's write refetches once more.
    setOrg({ ...currentOrg() });
    await flush();
    expect(getMock).toHaveBeenCalledTimes(3);
  });
});

describe('History', () => {
  const firstUrl = '/api/v1/home/history?limit=20';
  const nextUrl = '/api/v1/home/history?limit=20&before=' + encodeURIComponent(goHistory.next_before ?? '');
  const older = goHistory.threads[0];
  if (!older) throw new Error('fixture has no thread');
  const secondPage: HistoryResponse = {
    threads: [{ ...older, path: 'messages/2026-09-01/0001-buyer.md', request: { ...older.request, title: 'Renew the domain' } }],
    total: 41,
  };

  it('lists threads on the History tab and pages with Show more', async () => {
    desktop(true);
    await renderHome(busyHome, { history: { [firstUrl]: goHistory, [nextUrl]: secondPage } });
    fireEvent.mouseDown(screen.getByRole('tab', { name: 'History' }), { button: 0 });
    await flush();
    expect(screen.getByRole('heading', { name: 'History' })).toBeInTheDocument();
    expect(screen.getByText('41 threads')).toBeInTheDocument();
    const thread = screen.getByRole('button', { name: /Accept the hosting quote/ });
    expect(thread).toHaveTextContent('Approved');
    fireEvent.click(thread);
    expect(screen.getByText('Keep it under budget.')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Show more' }));
    await flush();
    expect(getMock).toHaveBeenCalledWith(nextUrl);
    expect(screen.getByRole('button', { name: /Renew the domain/ })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Show more' })).toBeNull();
  });

  it('opens from the foot link on phone with a back link to Home', async () => {
    await renderHome(busyHome, { path: '/?view=history', history: { [firstUrl]: goHistory } });
    expect(screen.getByRole('heading', { name: 'History' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Needs you' })).toBeNull();
  });
});
