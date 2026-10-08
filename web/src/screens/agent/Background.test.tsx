import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Background, BackgroundSession } from '../../api/types.gen';
import { goSnapshot } from '../../state/fixtures';
import { sessionTree } from './Background';
import { NOW, background, backgroundEmpty } from './tabsFixtures';
import { SLUG, deferred, renderTabs, withAgent } from './tabsHarness';

const PATH = '/agents/' + SLUG + '/background';
const API = '/api/v1/agents/' + SLUG + '/background';
/** The sub-lead in background.json, and not its worker, whose name carries the sub-lead's title too. */
const LEAD = /^\w+ ?2\. List every support incident/;

afterEach(() => {
  vi.unstubAllGlobals();
});

async function loaded() {
  await screen.findByText('Renewal recommendation: Acme');
}

/** A step's row: a button when it holds tasks, a plain row when it does not. */
function stepRow(title: RegExp): HTMLElement {
  const row = screen.getByText(title).closest('.kv-row');
  if (!(row instanceof HTMLElement)) throw new Error('no row for ' + String(title));
  return row;
}

describe('Background with a plan (background.json)', () => {
  it('heads the list with the plan, its progress and the counts', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: background } });
    await loaded();
    expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '1');
    expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuemax', '4');
    expect(screen.getByText(/1 of 4 steps/).closest('p')).toHaveTextContent('1 of 4 steps · 2 running · 2 finished · 1 needs help');
  });

  it('leaves the zero parts of the counts out', async () => {
    const quiet: Background = { plan: { ...background.plan!, running: 0, finished: 3, needs_help: 0 }, sessions: [] };
    renderTabs(PATH, { routes: { ['GET ' + API]: quiet } });
    await loaded();
    expect(screen.getByText(/1 of 4 steps/).closest('p')).toHaveTextContent(/^1 of 4 steps · 3 finished$/);
  });

  it('leads each step with its state, and opens only the running step', async () => {
    const user = userEvent.setup();
    renderTabs(PATH, { routes: { ['GET ' + API]: background } });
    await loaded();
    const expectState = (title: RegExp, label: string) => expect(within(stepRow(title)).getByRole('status', { name: label })).toBeInTheDocument();
    expectState(/1 · Pull actual spend/, 'Done');
    expectState(/2 · List every support incident/, 'Working');
    expectState(/3 · Find two comparable vendors/, 'Needs help');
    expectState(/4 · Write the recommendation/, 'Idle');
    // The running step shows its tasks; the step that needs help stays folded until it is opened.
    expect(stepRow(/2 · List every/)).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByRole('link', { name: LEAD })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Read the ticket export/ })).toBeInTheDocument();
    expect(stepRow(/3 · Find two/)).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByText('The pod ran out of memory')).toBeNull();
    await user.click(stepRow(/3 · Find two/));
    expect(screen.getByText('The pod ran out of memory')).toBeInTheDocument();
    await user.click(stepRow(/2 · List every/));
    expect(screen.queryByRole('link', { name: /Read the ticket export/ })).toBeNull();
  });

  it('says what each step holds', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: background } });
    await loaded();
    expect(within(stepRow(/2 · List every/)).getByText('1 running · 1 queued')).toBeInTheDocument();
    expect(within(stepRow(/3 · Find two/)).getByText('1 needs help')).toBeInTheDocument();
    expect(screen.getByText('Not started')).toBeInTheDocument();
  });

  it('indents a step’s tasks and lists the tasks of no step after them', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: background } });
    await loaded();
    const nested = screen.getByRole('link', { name: /Read the ticket export/ }).closest('.app-bg-task');
    expect(nested).toHaveClass('app-bg-task-nested');
    const other = screen.getByRole('heading', { name: 'Other background work' });
    expect(other).toBeInTheDocument();
    const loose = screen.getByRole('link', { name: /Check the contract dates/ }).closest('.app-bg-task');
    expect(loose).not.toHaveClass('app-bg-task-nested');
    // The unstepped group comes after every step.
    expect(other.compareDocumentPosition(stepRow(/4 · Write/)) & Node.DOCUMENT_POSITION_PRECEDING).toBeTruthy();
  });

  it('draws a task with its state, model badge, failure in words, elapsed time and a link to its transcript', async () => {
    const user = userEvent.setup();
    renderTabs(PATH, { routes: { ['GET ' + API]: background } });
    await loaded();
    const running = screen.getByRole('link', { name: LEAD });
    expect(within(running).getByRole('status', { name: 'Working' })).toBeInTheDocument();
    expect(within(running).getByText('Opus 5.5 · high')).toBeInTheDocument();
    expect(within(running).getByText('5:12')).toBeInTheDocument();
    expect(within(running).getByText('running file_view')).toBeInTheDocument();
    expect(running).toHaveAttribute('href', '/agents/engineering-lead/subagents/a1b2c3d4');
    expect(within(screen.getByRole('link', { name: /Read the ticket export/ })).getByRole('status', { name: 'Queued' })).toBeInTheDocument();
    await user.click(stepRow(/3 · Find two/));
    const failed = screen.getByRole('link', { name: /Find two comparable vendors.*The pod ran out of memory/ });
    expect(within(failed).getByText('1m')).toBeInTheDocument();
    expect(within(screen.getByRole('link', { name: /Check the contract dates/ })).getByText('40s')).toBeInTheDocument();
  });
});

describe('Background without a plan', () => {
  it('is the task rows alone, under a line that says there is no plan', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: { sessions: background.sessions } } });
    await screen.findByRole('heading', { name: 'No plan · 2 running · 2 finished' });
    expect(screen.queryByRole('progressbar')).toBeNull();
    for (const link of screen.getAllByRole('link', { name: /subagents|Read the ticket|List every|Find two|Check the/ })) {
      expect(link.closest('.app-bg-task')).not.toHaveClass('app-bg-task-nested');
    }
    expect(screen.getAllByRole('link', { name: /List every support incident|Read the ticket export|Find two comparable vendors|Check the contract dates/ })).toHaveLength(4);
  });

  it('says nothing is running when there are no sessions', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: backgroundEmpty } });
    expect(await screen.findByText('Nothing running in the background')).toBeInTheDocument();
    expect(screen.getByText(/starts show up here while they run/)).toBeInTheDocument();
  });

  it('names the failure and who can fix it when the read fails', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: () => new Response(JSON.stringify({ error: 'The agent could not be read.', who: 'Whoever runs this server can look into it.' }), { status: 500 }) } });
    expect(await screen.findByText('The agent could not be read.')).toBeInTheDocument();
  });
});

describe('Background counts a live task up between reads', () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  const elapsed = (name: RegExp) => screen.getByRole('link', { name }).querySelector('.app-bg-elapsed');

  it('carries a running and a queued task on from the read, and leaves an ended one where it ended', async () => {
    let now = NOW;
    renderTabs(PATH, { now: () => now, routes: { ['GET ' + API]: { sessions: background.sessions } } });
    await screen.findByRole('heading', { name: /No plan/ });
    expect(elapsed(LEAD)).toHaveTextContent('5:12');
    expect(elapsed(/Read the ticket export/)).toHaveTextContent('0:04');

    now += 1000;
    act(() => vi.advanceTimersByTime(1000));
    expect(elapsed(LEAD)).toHaveTextContent('5:13');
    expect(elapsed(/Read the ticket export/)).toHaveTextContent('0:05');

    // The clock is read, not counted: a tab that slept for a minute catches up on its next tick.
    now += 60_000;
    act(() => vi.advanceTimersByTime(1000));
    expect(elapsed(LEAD)).toHaveTextContent('6:13');
    expect(elapsed(/Find two comparable vendors/)).toHaveTextContent('1m');
    expect(elapsed(/Check the contract dates/)).toHaveTextContent('40s');
  });

  it('starts again from what the next read says', async () => {
    let now = NOW;
    let body: Background = { sessions: background.sessions };
    const h = renderTabs(PATH, { now: () => now, routes: { ['GET ' + API]: () => body } });
    await screen.findByRole('heading', { name: /No plan/ });
    now += 5000;
    act(() => vi.advanceTimersByTime(1000));
    expect(elapsed(LEAD)).toHaveTextContent('5:17');

    body = { sessions: background.sessions.map((s) => (s.id === 'a1b2c3d4' ? { ...s, elapsed_s: 400 } : s)) };
    h.snapshot(withAgent(goSnapshot, SLUG, { state: 'running' }));
    await waitFor(() => expect(elapsed(LEAD)).toHaveTextContent('6:40'));
    now += 2000;
    act(() => vi.advanceTimersByTime(1000));
    expect(elapsed(LEAD)).toHaveTextContent('6:42');
  });

  it('stops counting a task the next read says has ended', async () => {
    let now = NOW;
    let body: Background = { sessions: background.sessions };
    const h = renderTabs(PATH, { now: () => now, routes: { ['GET ' + API]: () => body } });
    await screen.findByRole('heading', { name: /No plan/ });
    body = { sessions: background.sessions.map((s) => (s.id === 'a1b2c3d4' ? { ...s, state: 'done', elapsed_s: 320, ended: '2026-09-22T17:00:08Z' } : s)) };
    h.snapshot(withAgent(goSnapshot, SLUG, { state: 'running' }));
    await waitFor(() => expect(elapsed(LEAD)).toHaveTextContent('5m'));
    now += 60_000;
    act(() => vi.advanceTimersByTime(1000));
    expect(elapsed(LEAD)).toHaveTextContent('5m');
  });
});

describe('Background follows the org snapshot', () => {
  it('reads again on every snapshot while the agent works, one read at a time', async () => {
    let calls = 0;
    const gates = [deferred<Background>(), deferred<Background>()];
    const h = renderTabs(PATH, {
      routes: {
        ['GET ' + API]: () => {
          calls++;
          return calls === 1 ? background : (gates[calls - 2]?.promise ?? background);
        },
      },
    });
    await loaded();
    expect(calls).toBe(1);
    // Three snapshots while the first re-read is still out: one read is in flight and one trails it.
    h.snapshot(withAgent(goSnapshot, SLUG, { state: 'running' }));
    await waitFor(() => expect(calls).toBe(2));
    h.snapshot(withAgent(goSnapshot, SLUG, { state: 'running' }));
    h.snapshot(withAgent(goSnapshot, SLUG, { state: 'waiting' }));
    expect(calls).toBe(2);
    await act(async () => gates[0]?.resolve(background));
    await waitFor(() => expect(calls).toBe(3));
    await act(async () => gates[1]?.resolve(background));
    expect(calls).toBe(3);
  });

  it('counts tasks still out as work in flight, whatever state the agent shows', async () => {
    const h = renderTabs(PATH, { routes: { ['GET ' + API]: background } });
    await loaded();
    h.snapshot(withAgent(goSnapshot, SLUG, { state: 'needs_help', waiting_tasks: 2 }));
    await waitFor(() => expect(h.count('GET', API)).toBe(2));
    h.snapshot(withAgent(goSnapshot, SLUG, { state: 'needs_help', waiting_tasks: 1 }));
    await waitFor(() => expect(h.count('GET', API)).toBe(3));
    // The last task comes back: one trailing read, then quiet.
    h.snapshot(withAgent(goSnapshot, SLUG, { state: 'needs_help', waiting_tasks: 0 }));
    await waitFor(() => expect(h.count('GET', API)).toBe(4));
    h.snapshot(withAgent(goSnapshot, SLUG, { state: 'needs_help', waiting_tasks: 0, context_pct: 50 }));
    await act(async () => {});
    expect(h.count('GET', API)).toBe(4);
  });

  it('reads nothing on snapshots while nothing is in flight', async () => {
    const idle = withAgent(goSnapshot, SLUG, { state: 'idle', waiting_tasks: 0 });
    const h = renderTabs(PATH, { snapshot: idle, routes: { ['GET ' + API]: background } });
    await loaded();
    h.snapshot(idle);
    h.snapshot(withAgent(idle, 'chief-of-staff', { state: 'running' }));
    await act(async () => {});
    expect(h.count('GET', API)).toBe(1);
  });

  it('reads once more on the snapshot where the work stops, then stays quiet', async () => {
    let body: Background = background;
    const h = renderTabs(PATH, { routes: { ['GET ' + API]: () => body } });
    await loaded();
    h.snapshot(withAgent(goSnapshot, SLUG, { state: 'waiting' }));
    await waitFor(() => expect(h.count('GET', API)).toBe(2));
    // The last task finishes: the trailing read shows nothing left.
    body = backgroundEmpty;
    h.snapshot(withAgent(goSnapshot, SLUG, { state: 'idle' }));
    expect(await screen.findByText('Nothing running in the background')).toBeInTheDocument();
    expect(h.count('GET', API)).toBe(3);
    h.snapshot(withAgent(goSnapshot, SLUG, { state: 'idle' }));
    h.snapshot(withAgent(goSnapshot, SLUG, { state: 'idle', context_pct: 12 }));
    expect(h.count('GET', API)).toBe(3);
  });
});

describe('Background draws a task under the task that dispatched it', () => {
  const task = (id: string, title: string, caller_id?: string): BackgroundSession => ({ ...background.sessions[3]!, id, title, caller_id });
  const row = (name: RegExp) => screen.getByRole('link', { name }).closest('.app-bg-task') as HTMLElement;

  it('sits the worker in the golden file one level under its sub-lead, and names the sub-lead to a screen reader', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: background } });
    await loaded();
    const lead = row(LEAD);
    const worker = row(/Read the ticket export/);
    expect(lead).toHaveAttribute('data-depth', '0');
    expect(lead).not.toHaveClass('app-bg-task-child');
    expect(lead.querySelector('.app-bg-stem')).not.toBeNull();
    expect(worker).toHaveAttribute('data-depth', '1');
    expect(worker).toHaveClass('app-bg-task-nested', 'app-bg-task-child');
    expect(worker.querySelector('.app-bg-elbow')).not.toBeNull();
    expect(lead.nextElementSibling).toBe(worker);
    expect(screen.getByRole('link', { name: /Read the ticket export, a task of 2\. List every support incident/ })).toBeInTheDocument();
  });

  it('draws the tree however the server ordered it', async () => {
    // A worker sent before its caller, one two levels deep, and a sibling after it.
    const sessions = [task('w1', 'Worker one', 'lead'), task('lead', 'Lead'), task('w2', 'Worker two', 'lead'), task('g1', 'Grandchild', 'w1'), task('solo', 'Solo')];
    renderTabs(PATH, { routes: { ['GET ' + API]: { sessions } } });
    await screen.findByRole('heading', { name: /No plan/ });
    const order = [...document.querySelectorAll('.app-bg-task')].map((el) => [el.querySelector('.kv-row-title')?.firstChild?.textContent, el.getAttribute('data-depth')]);
    expect(order).toEqual([
      ['Lead', '0'],
      ['Worker one', '1'],
      ['Grandchild', '2'],
      ['Worker two', '1'],
      ['Solo', '0'],
    ]);
    // Worker one has a sibling after it, so the lead's line runs on past it and past its own worker.
    expect(row(/^\w+ ?Worker one/).querySelectorAll('.app-bg-rail')).toHaveLength(1);
    expect(row(/^\w+ ?Grandchild/).querySelectorAll('.app-bg-rail')).toHaveLength(1);
    // Worker two is the lead's last: its line ends in the elbow.
    expect(row(/^\w+ ?Worker two/).querySelectorAll('.app-bg-rail')).toHaveLength(0);
    expect(row(/^\w+ ?Solo/).querySelector('.app-bg-rails')).toBeNull();
  });
});

describe('sessionTree', () => {
  const s = (id: string, caller_id?: string): BackgroundSession => ({ ...background.sessions[3]!, id, title: id, caller_id });
  const shape = (list: BackgroundSession[]) => sessionTree(list).map((p) => ({ id: p.session.id, depth: p.depth, rails: p.rails, last: p.last, kids: p.hasChildren, parent: p.parent?.id }));

  it('keeps a flat list flat and in order', () => {
    expect(shape([s('a'), s('b')]).map((p) => [p.id, p.depth])).toEqual([
      ['a', 0],
      ['b', 0],
    ]);
  });

  it('carries each branch’s line down while a later sibling is still to come', () => {
    expect(shape([s('r'), s('a', 'r'), s('b', 'r'), s('a1', 'a'), s('b1', 'b')])).toEqual([
      { id: 'r', depth: 0, rails: [], last: true, kids: true, parent: undefined },
      { id: 'a', depth: 1, rails: [true], last: false, kids: true, parent: 'r' },
      { id: 'a1', depth: 2, rails: [true, false], last: true, kids: false, parent: 'a' },
      { id: 'b', depth: 1, rails: [false], last: true, kids: true, parent: 'r' },
      { id: 'b1', depth: 2, rails: [false, false], last: true, kids: false, parent: 'b' },
    ]);
  });

  it('heads a tree with a task whose caller is not in the group, and breaks a caller loop', () => {
    expect(shape([s('orphan', 'gone'), s('x', 'y'), s('y', 'x'), s('self', 'self')]).map((p) => [p.id, p.depth])).toEqual([
      ['orphan', 0],
      ['self', 0],
      ['x', 0],
      ['y', 1],
    ]);
  });
});
