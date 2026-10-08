import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, apiGet, apiPost } from '../../api/client';
import type { Assignment as AssignmentData } from '../../api/types.gen';
import { TooltipProvider } from '../../ds';
import { Assignment } from './Assignment';
import { closedAssignment, goAssignment, heldHereAssignment, openAssignment } from './fixtures';
import { desktop, flush, goOrg, setOrg, useOrgStub } from './testOrg';

vi.mock('../../api/client', async (importOriginal) => {
  const real = await importOriginal<typeof import('../../api/client')>();
  return { ...real, apiGet: vi.fn(), apiPost: vi.fn() };
});

vi.mock('../../state/OrgProvider', () => ({ useOrg: () => useOrgStub(), usePersonName: () => 'You' }));

const getMock = vi.mocked(apiGet);
const postMock = vi.mocked(apiPost);
const clock = () => Date.parse('2026-09-20T18:00:00Z');

function serve(a: AssignmentData) {
  getMock.mockImplementation(async (path: string) => {
    if (path === '/api/v1/assignments/42') return structuredClone(a);
    throw new ApiError(404, 'assignment not found', 'Check the number.');
  });
}

async function renderAssignment(a: AssignmentData = goAssignment) {
  serve(a);
  setOrg(goOrg());
  const router = createMemoryRouter([{ path: '/assignments/:id', element: <Assignment clock={clock} /> }], { basename: '/', initialEntries: ['/assignments/42'] });
  render(
    <TooltipProvider>
      <RouterProvider router={router} />
    </TooltipProvider>,
  );
  await flush();
}

/** Opens the Act menu and picks an item. */
async function act_(item: string) {
  const trigger = screen.getByRole('button', { name: 'Act' });
  fireEvent.keyDown(trigger, { key: 'Enter' });
  await flush();
  fireEvent.click(screen.getByRole('menuitem', { name: item }));
  await flush();
}

const dialog = () => screen.getByRole('dialog');

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] });
  vi.setSystemTime(new Date('2026-09-20T18:00:00Z'));
  desktop(true);
  getMock.mockReset();
  postMock.mockReset().mockResolvedValue({});
});

afterEach(() => {
  vi.useRealTimers();
});

describe('the page', () => {
  it('names the state, the number and why it cannot move, in words', async () => {
    await renderAssignment();
    expect(screen.getByRole('heading', { level: 1, name: 'Update the docs' })).toBeInTheDocument();
    expect(screen.getByRole('status', { name: '' })).toHaveTextContent('On hold');
    expect(screen.getAllByText('#42')[0]).toBeInTheDocument();
    expect(screen.getByText('On hold under #40 by you.')).toBeInTheDocument();
  });

  it('draws all eight facts on desktop, in the canvas order', async () => {
    await renderAssignment();
    const labels = Array.from(document.querySelectorAll('.app-work-facts dt')).map((dt) => dt.textContent);
    expect(labels).toEqual(['Assignee', 'Opened by', 'Part of', 'Counts toward', 'Parts', 'Waits on', 'Holds up', 'Dates']);
    expect(screen.queryByText('More details')).not.toBeInTheDocument();
    const facts = document.querySelector('.app-work-facts') as HTMLElement;
    const fact = (label: string) => within(facts).getByText(label, { selector: 'dt' }).parentElement as HTMLElement;
    // Part of is the one titled ref; Parts, Waits on, Holds up and Counts toward are bare #id refs, as the canvas draws them.
    expect(within(fact('Part of')).getByRole('link', { name: /#40\s*Ship the release/ })).toHaveAttribute('href', '/assignments/40');
    expect(within(fact('Waits on')).getByRole('link', { name: '#45' })).toHaveAttribute('href', '/assignments/45');
    expect(within(fact('Holds up')).getByRole('link', { name: '#40' })).toHaveAttribute('href', '/assignments/40');
    expect(within(fact('Parts')).getByRole('link', { name: '#47' })).toHaveAttribute('href', '/assignments/47');
    expect(fact('Counts toward')).toHaveTextContent('docs updated, on #40');
    // Updated an hour before the clock: the same day in most zones, so only the time is said.
    expect(screen.getByText(/^Opened 1 Sept · updated (\d\d:\d\d|\d+ Sept \d\d:\d\d)$/)).toBeInTheDocument();
  });

  it('sets Dates in the caption type every other fact value uses', async () => {
    const { part_of: _partOf, ...topLevel } = goAssignment.facts;
    void _partOf;
    await renderAssignment({ ...goAssignment, facts: { ...topLevel, waits_on: [] } });
    const dates = screen.getByText(/^Opened 1 Sept · updated/);
    expect(dates).toHaveClass('kv-text-caption');
    expect(dates.closest('.app-work-fact-value')).not.toBeNull();
    // The placeholders beside it (Top level, Nothing) are the same Text variant.
    expect(screen.getByText('Top level')).toHaveClass('kv-text-caption');
    expect(screen.getByText('Nothing')).toHaveClass('kv-text-caption');
  });

  it('draws every part, twelve of them, as bare refs in one wrapping row', async () => {
    const twelve = Array.from({ length: 12 }, (_, i) => ({ ...(goAssignment.parts[0] as AssignmentData['parts'][number]), id: 100 + i, title: 'Part number ' + (i + 1) + ' with a long title' }));
    await renderAssignment({ ...goAssignment, parts: twelve });
    const facts = document.querySelector('.app-work-facts') as HTMLElement;
    const parts = within(facts).getByText('Parts', { selector: 'dt' }).parentElement as HTMLElement;
    const links = within(parts).getAllByRole('link');
    expect(links.map((l) => l.textContent)).toEqual(twelve.map((p) => '#' + p.id));
    expect(links[11]).toHaveAttribute('href', '/assignments/111');
    expect(parts.querySelectorAll('.app-work-refs')).toHaveLength(1);
  });

  it('puts the Act menu beside the title block on desktop and on the state line on a phone', async () => {
    await renderAssignment();
    const act = screen.getByRole('button', { name: 'Act' });
    expect(act.closest('.app-work-detail-line')).toBeNull();
    expect(act.closest('.app-work-detail-head')).not.toBeNull();
    expect(screen.getByText(/part of #40 Ship the release/)).toBeInTheDocument();
  });

  it('keeps Act on the state line on a phone, with the number alone', async () => {
    desktop(false);
    await renderAssignment();
    expect(screen.getByRole('button', { name: 'Act' }).closest('.app-work-detail-line')).not.toBeNull();
    expect(screen.queryByText(/part of #40/)).not.toBeInTheDocument();
  });

  it('shows four facts on a phone and folds the rest under More details', async () => {
    desktop(false);
    await renderAssignment();
    for (const label of ['Assignee', 'Part of', 'Waits on', 'Holds up']) expect(screen.getByText(label)).toBeVisible();
    const folded = screen.getByText('Opened by');
    expect(folded).not.toBeVisible();
    for (const label of ['Counts toward', 'Parts', 'Dates']) expect(screen.getByText(label)).not.toBeVisible();
    fireEvent.click(screen.getByText('More details'));
    expect(screen.getByText('Opened by')).toBeVisible();
  });

  it('renders the description as markdown', async () => {
    await renderAssignment();
    const heading = screen.getByRole('heading', { name: 'Description' });
    expect(within(heading.parentElement as HTMLElement).getByText('v3')).toHaveProperty('tagName', 'STRONG');
  });

  it('draws Done when as a meter and a Condition, State, Met by table', async () => {
    await renderAssignment();
    expect(screen.getByRole('img', { name: '1 of 3 met · 1 in progress · 1 unclaimed' })).toBeInTheDocument();
    const table = screen.getByRole('table');
    expect(within(table).getAllByRole('columnheader').map((h) => h.textContent)).toEqual(['Condition', 'State', 'Met by']);
    const rows = within(table).getAllByRole('row').slice(1);
    expect(within(rows[0] as HTMLElement).getByText('Met')).toBeInTheDocument();
    expect(within(rows[0] as HTMLElement).getByRole('link', { name: /#47\s*List the pages/ })).toHaveAttribute('href', '/assignments/47');
    expect(within(rows[1] as HTMLElement).getByText('In progress')).toBeInTheDocument();
    expect(within(rows[1] as HTMLElement).getByRole('link', { name: /#48\s*Margins/ })).toHaveAttribute('href', '/assignments/48');
    expect(within(rows[1] as HTMLElement).getByText('working')).toBeInTheDocument();
    expect(within(rows[2] as HTMLElement).getByText('Unclaimed')).toBeInTheDocument();
    expect(within(rows[2] as HTMLElement).getByText('Nobody yet')).toBeInTheDocument();
  });

  it('writes the outcome', async () => {
    await renderAssignment();
    expect(screen.getByRole('heading', { name: 'Outcome' })).toBeInTheDocument();
    expect(screen.getByText('Every page is up to date.')).toBeInTheDocument();
    expect(screen.getByText('Not met when it closed: sign-off.')).toBeInTheDocument();
  });

  it('says the outcome is still to come while it is open', async () => {
    await renderAssignment(openAssignment);
    expect(screen.getByRole('heading', { name: 'Outcome' })).toBeInTheDocument();
    expect(screen.getByText('Not closed yet. The outcome is written when Buyer or you close it.')).toBeInTheDocument();
    expect(screen.queryByText('On hold under #40 by you.')).not.toBeInTheDocument();
  });

  it('writes the log in plain English with before links', async () => {
    await renderAssignment();
    const log = screen.getByRole('list');
    const rows = within(log).getAllByRole('listitem');
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveTextContent('Engineering lead opened it, assigned to Buyer');
    expect(within(rows[1] as HTMLElement).getByRole('link', { name: '#45' })).toHaveAttribute('href', '/assignments/45');
    expect(rows[2]).toHaveTextContent('You edited the title');
    expect(rows[2]).toHaveTextContent('“Wait for the review.”');
    const before = within(rows[2] as HTMLElement).getByRole('link', { name: 'before' });
    expect(before).toHaveAttribute('href', '/attachments/3f2a');
    expect(before).toHaveAttribute('target', '_blank');
    expect(within(rows[0] as HTMLElement).queryByRole('link', { name: 'before' })).not.toBeInTheDocument();
  });

  it('says what happened when the assignment cannot be loaded', async () => {
    getMock.mockRejectedValue(new ApiError(404, 'assignment not found', 'Check the number.'));
    setOrg(goOrg());
    const router = createMemoryRouter([{ path: '/assignments/:id', element: <Assignment clock={clock} /> }], { basename: '/', initialEntries: ['/assignments/42'] });
    render(
      <TooltipProvider>
        <RouterProvider router={router} />
      </TooltipProvider>,
    );
    await flush();
    expect(screen.getByRole('alert')).toHaveTextContent('Assignment not found.');
  });

  it('refetches when the org snapshot reports a new assignments_version', async () => {
    await renderAssignment();
    expect(getMock).toHaveBeenCalledTimes(1);
    setOrg(goOrg({ assignmentsVersion: 99 }));
    await flush();
    expect(getMock).toHaveBeenCalledTimes(2);
  });
});

describe('the Act menu', () => {
  async function items(a: AssignmentData) {
    await renderAssignment(a);
    fireEvent.keyDown(screen.getByRole('button', { name: 'Act' }), { key: 'Enter' });
    await flush();
    const state = (name: string) => screen.getByRole('menuitem', { name }).getAttribute('aria-disabled') === 'true';
    return { disabled: state, has: (name: string) => screen.queryByRole('menuitem', { name }) !== null };
  }

  it('offers Close, Put on hold and Edit while open; Reopen waits for a close', async () => {
    const m = await items(openAssignment);
    expect(m.disabled('Close')).toBe(false);
    expect(m.disabled('Put on hold')).toBe(false);
    expect(m.disabled('Edit')).toBe(false);
    expect(m.disabled('Reopen')).toBe(true);
    expect(m.has('Release hold')).toBe(false);
  });

  it('offers only Reopen once closed', async () => {
    const m = await items(closedAssignment);
    expect(m.disabled('Close')).toBe(true);
    expect(m.disabled('Put on hold')).toBe(true);
    expect(m.disabled('Edit')).toBe(true);
    expect(m.disabled('Reopen')).toBe(false);
  });

  it('swaps Put on hold for Release hold when the hold is its own', async () => {
    const m = await items(heldHereAssignment);
    expect(m.has('Put on hold')).toBe(false);
    expect(m.disabled('Release hold')).toBe(false);
  });
});

describe('the Act dialogs', () => {
  it('Close asks for a resolution and an outcome and posts both', async () => {
    await renderAssignment();
    await act_('Close');
    const d = within(dialog());
    expect(d.getByRole('heading', { name: 'Close #42?' })).toBeInTheDocument();
    expect(dialog()).toHaveTextContent('Buyer and Engineering lead are told at once.');
    expect(d.getByRole('button', { name: 'Cancel' })).toBeInTheDocument();
    const primary = d.getByRole('button', { name: 'Close assignment' });
    expect(primary).toBeDisabled();
    fireEvent.change(d.getByLabelText('Resolution'), { target: { value: 'dropped' } });
    fireEvent.change(d.getByLabelText('Outcome'), { target: { value: 'Swapped U7 on every board.' } });
    expect(primary).toBeEnabled();
    await act(async () => {
      primary.click();
    });
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/assignments/42/close', { resolution: 'dropped', outcome: 'Swapped U7 on every board.' });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.getByText('Assignment closed')).toBeInTheDocument();
    expect(getMock).toHaveBeenCalledTimes(2);
  });

  it('Put on hold needs a note and posts held true', async () => {
    await renderAssignment(openAssignment);
    await act_('Put on hold');
    const d = within(dialog());
    expect(d.getByRole('heading', { name: 'Put #42 on hold?' })).toBeInTheDocument();
    const primary = d.getByRole('button', { name: 'Put on hold' });
    expect(primary).toBeDisabled();
    fireEvent.change(d.getByLabelText('Note'), { target: { value: 'Wait for the review.' } });
    await act(async () => {
      primary.click();
    });
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/assignments/42/hold', { held: true, note: 'Wait for the review.' });
    expect(screen.getByText('Put on hold')).toBeInTheDocument();
  });

  it('Release hold posts held false', async () => {
    await renderAssignment(heldHereAssignment);
    await act_('Release hold');
    const d = within(dialog());
    fireEvent.change(d.getByLabelText('Note'), { target: { value: 'Review done.' } });
    await act(async () => {
      d.getByRole('button', { name: 'Release hold' }).click();
    });
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/assignments/42/hold', { held: false, note: 'Review done.' });
  });

  it('Reopen posts the note', async () => {
    await renderAssignment(closedAssignment);
    await act_('Reopen');
    const d = within(dialog());
    fireEvent.change(d.getByLabelText('Note'), { target: { value: 'Tests ran on staging.' } });
    await act(async () => {
      d.getByRole('button', { name: 'Reopen' }).click();
    });
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/assignments/42/reopen', { note: 'Tests ran on staging.' });
  });

  it('Edit posts only what changed, with the seq it was opened against', async () => {
    await renderAssignment(openAssignment);
    await act_('Edit');
    const d = within(dialog());
    expect(d.getByLabelText('Title')).toHaveValue('Update the docs');
    expect(d.getByLabelText('Description')).toHaveValue('Check the docs against the **v3** API.');
    // Assignees come from the team tree; the current one is selected.
    expect(d.getByLabelText('Assignee')).toHaveValue('buyer');
    expect(within(d.getByLabelText('Assignee')).getAllByRole('option').map((o) => o.textContent)).toEqual(['Chief of Staff', 'Engineering lead', 'Buyer', 'tester']);
    const primary = d.getByRole('button', { name: 'Save changes' });
    expect(primary).toBeDisabled();
    fireEvent.change(d.getByLabelText('Title'), { target: { value: 'Update the docs, v3' } });
    fireEvent.change(d.getByLabelText('Assignee'), { target: { value: 'engineering-lead' } });
    fireEvent.change(d.getByLabelText('Note'), { target: { value: 'Yours now.' } });
    await act(async () => {
      primary.click();
    });
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/assignments/42/update', { seq: 118, title: 'Update the docs, v3', assignee: 'engineering-lead', note: 'Yours now.' });
    expect(screen.getByText('Changes saved')).toBeInTheDocument();
  });

  it('on a stale seq says so and reloads on request', async () => {
    postMock.mockRejectedValue(new ApiError(409, 'the assignment changed', 'Reload it.'));
    await renderAssignment(openAssignment);
    await act_('Edit');
    const d = within(dialog());
    fireEvent.change(d.getByLabelText('Description'), { target: { value: 'New text.' } });
    await act(async () => {
      d.getByRole('button', { name: 'Save changes' }).click();
    });
    await flush();
    const banner = within(dialog()).getByRole('alert');
    expect(banner).toHaveTextContent('This assignment changed while you were editing.');
    expect(banner).toHaveTextContent('Reload to see the latest.');
    expect(getMock).toHaveBeenCalledTimes(1);
    await act(async () => {
      within(banner).getByRole('button', { name: 'Reload' }).click();
    });
    await flush();
    expect(getMock).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('shows any other refusal in the dialog and keeps what was typed', async () => {
    postMock.mockRejectedValue(new ApiError(400, 'a note is required when the change wakes someone', 'Say why in the note.'));
    await renderAssignment(openAssignment);
    await act_('Edit');
    const d = within(dialog());
    fireEvent.change(d.getByLabelText('Assignee'), { target: { value: 'tester' } });
    await act(async () => {
      d.getByRole('button', { name: 'Save changes' }).click();
    });
    await flush();
    expect(within(dialog()).getByRole('alert')).toHaveTextContent('A note is required when the change wakes someone.');
    expect(within(dialog()).getByLabelText('Assignee')).toHaveValue('tester');
    expect(within(dialog()).queryByRole('button', { name: 'Reload' })).not.toBeInTheDocument();
  });

  it('Cancel closes without posting', async () => {
    await renderAssignment();
    await act_('Close');
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Cancel' }));
    await flush();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(postMock).not.toHaveBeenCalled();
  });
});
