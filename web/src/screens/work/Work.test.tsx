import { render, screen, within } from '@testing-library/react';
import { act } from '@testing-library/react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, apiGet } from '../../api/client';
import type { WorkBoard } from '../../api/types.gen';
import { TooltipProvider } from '../../ds';
import { Work } from './Work';
import { emptyWork, goWork } from './fixtures';
import { desktop, flush, goOrg, setOrg, useOrgStub } from './testOrg';

vi.mock('../../api/client', async (importOriginal) => {
  const real = await importOriginal<typeof import('../../api/client')>();
  return { ...real, apiGet: vi.fn() };
});

vi.mock('../../state/OrgProvider', () => ({ useOrg: () => useOrgStub(), usePersonName: () => 'You' }));

const getMock = vi.mocked(apiGet);

function serve(board: WorkBoard) {
  getMock.mockImplementation(async (path: string) => {
    if (path === '/api/v1/work') return structuredClone(board);
    throw new ApiError(404, 'not found', 'Nobody');
  });
}

async function renderWork(board: WorkBoard, path = '/work') {
  serve(board);
  setOrg(goOrg());
  const router = createMemoryRouter([{ path: '/work', element: <Work /> }], { basename: '/', initialEntries: [path] });
  const utils = render(
    <TooltipProvider>
      <RouterProvider router={router} />
    </TooltipProvider>,
  );
  await flush();
  return utils;
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] });
  vi.setSystemTime(new Date('2026-09-20T17:00:00Z'));
  desktop(true);
  getMock.mockReset();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('the board', () => {
  it('reads by goal: header, progress and the three columns', async () => {
    await renderWork(goWork);
    const goal = screen.getByRole('region', { name: 'Ship the release' });
    expect(within(goal).getByRole('link', { name: 'Ship the release' })).toHaveAttribute('href', '/assignments/40');
    expect(within(goal).getByText('#40')).toBeInTheDocument();
    expect(within(goal).getByText('1 of 3')).toBeInTheDocument();
    expect(within(goal).getByRole('progressbar')).toHaveAttribute('aria-valuenow', '1');
    // The goal's owner sits in the header, beside the name.
    expect(within(goal).getAllByRole('img', { name: 'Engineering lead' }).length).toBeGreaterThan(0);
    for (const name of ['Look back', 'Current', 'Look forward']) expect(within(goal).getByRole('region', { name })).toBeInTheDocument();
  });

  it('links the readouts to the filtered board', async () => {
    await renderWork(goWork);
    expect(screen.getByRole('link', { name: '14 open assignments' })).toHaveAttribute('href', '/work');
    expect(screen.getByRole('link', { name: '5 ready' })).toHaveAttribute('href', '/work?state=ready');
    expect(screen.getByRole('link', { name: '2 blocked' })).toHaveAttribute('href', '/work?state=blocked');
    expect(screen.getByRole('link', { name: '1 on hold' })).toHaveAttribute('href', '/work?state=on_hold');
    expect(screen.getByRole('link', { name: '9 closed this week' })).toHaveAttribute('href', '/work?closed=week');
  });

  it('groups each column under its owners and says why a row cannot move', async () => {
    await renderWork(goWork);
    const current = screen.getByRole('region', { name: 'Current' });
    const owners = within(current)
      .getAllByText(/^(Buyer|Engineering lead)$/)
      .map((el) => el.textContent);
    expect(owners).toEqual(['Buyer', 'Engineering lead']);

    const blocked = within(current).getByRole('link', { name: /Update the docs/ });
    expect(blocked).toHaveAttribute('href', '/assignments/42');
    // The reason in words: the ref and the title of what it waits on, from waiting_on[].
    expect(blocked.querySelector('.kv-irow-why')).toHaveTextContent(/^Waiting on #45, Run the tests$/);
    expect(within(blocked).getByRole('img', { name: '1 of 3 met · 1 in progress · 1 unclaimed' })).toBeInTheDocument();
    expect(within(current).getByRole('link', { name: /Fixture/ })).toHaveTextContent('On hold by you');
    expect(within(current).getByRole('link', { name: /Migration/ })).not.toHaveTextContent('Waiting on');
    // The compact run state of each owner that is doing something.
    expect(within(current).getByRole('status', { name: 'Working' })).toBeInTheDocument();
    expect(within(current).getByRole('status', { name: 'Needs help' })).toBeInTheDocument();
    expect(within(current).getByText('1 moving · 1 blocked · 1 on hold')).toBeInTheDocument();
  });

  it('names who holds a row when the server gives no sentence', async () => {
    const board = structuredClone(goWork);
    const fixture = board.goals[0]?.current.find((i) => i.id === 43);
    if (!fixture) throw new Error('fixture lost item 43');
    delete fixture.why;
    fixture.held_by = { slug: 'maya', name: 'Maya' };
    await renderWork(board);
    expect(screen.getByRole('link', { name: /Fixture/ }).querySelector('.kv-irow-why')).toHaveTextContent(/^On hold by Maya$/);
  });

  it('fills Look back and ends Look forward with the unclaimed conditions', async () => {
    await renderWork(goWork);
    const back = screen.getByRole('region', { name: 'Look back' });
    expect(within(back).getByRole('link', { name: /Draft the release notes/ })).toBeInTheDocument();
    const forward = screen.getByRole('region', { name: 'Look forward' });
    expect(within(forward).getByRole('link', { name: /Label/ })).toBeInTheDocument();
    const foot = within(forward).getByText('test plan').parentElement as HTMLElement;
    expect(foot).toHaveClass('app-work-unclaimed');
    expect(within(foot).getByText('Unclaimed')).toHaveClass('app-work-unclaimed-word');
    expect(within(forward).getByText('ready · 1 unclaimed')).toBeInTheDocument();
  });

  it('keeps recently closed goals in a card that starts folded', async () => {
    await renderWork(goWork);
    expect(screen.getByRole('heading', { name: 'Recently closed goals' })).toBeInTheDocument();
    expect(screen.getByText('1 this week')).toBeInTheDocument();
    const row = screen.getByRole('link', { name: /Pick a host/ });
    expect(row).toHaveAttribute('href', '/assignments/30');
    expect(row).not.toBeVisible();
  });

  it('opens the closed card and scrolls to it for ?closed=week', async () => {
    const scroll = vi.spyOn(Element.prototype, 'scrollIntoView');
    await renderWork(goWork, '/work?closed=week');
    expect(screen.getByRole('link', { name: /Pick a host/ })).toBeVisible();
    expect(scroll).toHaveBeenCalled();
    scroll.mockRestore();
  });

  it('narrows to one state for ?state=', async () => {
    await renderWork(goWork, '/work?state=blocked');
    expect(screen.getByRole('link', { name: /Update the docs/ })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /Fixture/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /Draft the release notes/ })).not.toBeInTheDocument();
    expect(screen.queryByText('test plan')).not.toBeInTheDocument();
    expect(screen.getByText('Showing blocked assignments only.')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Show all' })).toHaveAttribute('href', '/work');
  });

  it('keeps a goal that has something in the state asked for', async () => {
    await renderWork(goWork, '/work?state=ready');
    // Look forward holds one ready row, so the goal stays; the blocked row goes.
    expect(screen.getByRole('link', { name: /Label/ })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /Update the docs/ })).not.toBeInTheDocument();
  });

  it('shows the empty state and no File button when there are no goals', async () => {
    await renderWork(emptyWork);
    expect(screen.getByRole('heading', { name: 'No goals yet' })).toBeInTheDocument();
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });

  it('stacks the columns on a phone the same way', async () => {
    desktop(false);
    await renderWork(goWork);
    expect(screen.getByRole('region', { name: 'Current' })).toBeInTheDocument();
  });
});

describe('loading and errors', () => {
  it('draws skeletons only once loading has taken a moment', async () => {
    getMock.mockImplementation(() => new Promise(() => {}));
    setOrg(goOrg());
    const router = createMemoryRouter([{ path: '/work', element: <Work /> }], { basename: '/', initialEntries: ['/work'] });
    render(
      <TooltipProvider>
        <RouterProvider router={router} />
      </TooltipProvider>,
    );
    await flush();
    expect(screen.queryByLabelText('Loading Work')).not.toBeInTheDocument();
    await act(async () => {
      vi.advanceTimersByTime(300);
    });
    expect(screen.getByLabelText('Loading Work')).toHaveAttribute('aria-busy', 'true');
  });

  it('says what happened and who can fix it, and tries again', async () => {
    getMock.mockRejectedValueOnce(new ApiError(503, 'the tracker is unavailable', 'Whoever runs this Kivali server can look into it.'));
    setOrg(goOrg());
    const router = createMemoryRouter([{ path: '/work', element: <Work /> }], { basename: '/', initialEntries: ['/work'] });
    render(
      <TooltipProvider>
        <RouterProvider router={router} />
      </TooltipProvider>,
    );
    await flush();
    const banner = screen.getByRole('alert');
    expect(banner).toHaveTextContent('The tracker is unavailable.');
    expect(banner).toHaveTextContent('Whoever runs this Kivali server can look into it.');
    serve(goWork);
    await act(async () => {
      screen.getByRole('button', { name: 'Try again' }).click();
    });
    await flush();
    expect(screen.getByRole('region', { name: 'Ship the release' })).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('refetches when the org snapshot reports a new assignments_version', async () => {
    await renderWork(goWork);
    expect(getMock).toHaveBeenCalledTimes(1);
    setOrg(goOrg());
    await flush();
    expect(getMock).toHaveBeenCalledTimes(1);
    setOrg(goOrg({ assignmentsVersion: 99 }));
    await flush();
    expect(getMock).toHaveBeenCalledTimes(2);
  });
});
