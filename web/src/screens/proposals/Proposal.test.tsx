import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, apiGet, apiPostForm } from '../../api/client';
import type { NeedActionResponse, Proposal as ProposalData } from '../../api/types.gen';
import { FrameChromeContext } from '../../app/chrome';
import type { FrameChrome } from '../../app/chrome';
import { pickFiles } from '../../lib/pickFiles';
import { handbook, goOffboard, goReorg, goRoleUpdate, hire } from './fixtures';
import { Proposal } from './Proposal';

vi.mock('../../api/client', async (importOriginal) => {
  const real = await importOriginal<typeof import('../../api/client')>();
  return { ...real, apiGet: vi.fn(), apiPostForm: vi.fn() };
});
vi.mock('../../lib/pickFiles', () => ({ pickFiles: vi.fn() }));

const getMock = vi.mocked(apiGet);
const formMock = vi.mocked(apiPostForm);
const pickMock = vi.mocked(pickFiles);

/** Three hours after the hire fixture was proposed. */
const NOW = Date.parse('2026-09-28T12:00:00Z');

async function flush() {
  await act(async () => {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  });
}

async function renderProposal(p: ProposalData, chromeSpy?: (c: FrameChrome | null) => void) {
  getMock.mockResolvedValue(structuredClone(p));
  const router = createMemoryRouter(
    [
      { path: '/', element: <p>home page</p> },
      { path: '/team', element: <p>team page</p> },
      { path: '/agents/:slug', element: <p>agent page</p> },
      { path: '/proposals/*', element: <Proposal now={NOW} /> },
    ],
    { basename: '/', initialEntries: ['/proposals/' + p.path] },
  );
  const ui = <RouterProvider router={router} />;
  const utils = render(chromeSpy ? <FrameChromeContext.Provider value={{ setChrome: chromeSpy }}>{ui}</FrameChromeContext.Provider> : ui);
  await flush();
  return { ...utils, router };
}

function section(title: string): HTMLDetailsElement {
  const el = screen.getByText(title, { selector: 'h3' }).closest('details');
  if (!el) throw new Error('no section ' + title);
  return el;
}

beforeEach(() => {
  getMock.mockReset();
  formMock.mockReset();
  pickMock.mockReset();
});

describe('Proposal page', () => {
  it('makes the title the page h1 from 960', async () => {
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: q.includes('min-width'), media: q, addEventListener: () => {}, removeEventListener: () => {} }));
    try {
      await renderProposal(hire);
      expect(screen.getByRole('heading', { level: 1, name: 'Hire a newsletter editor' })).toHaveClass('app-proposal-title');
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('reads a hire: header, reason, agent tile, facts, and new documents with no diff', async () => {
    await renderProposal(hire);
    // Below 960 (the default here) the frame's phone header carries the h1; the title is a paragraph.
    expect(screen.getByText('Hire a newsletter editor', { selector: '.app-proposal-title' }).tagName).toBe('P');
    expect(screen.queryByRole('heading', { level: 1 })).toBeNull();
    expect(screen.getByText('Hire')).toBeTruthy();
    expect(screen.getAllByText('Needs your approval')).toHaveLength(1);
    expect(screen.getByText('Proposed by Chief of Staff · 3h ago')).toBeTruthy();
    expect(screen.getByText('draft').tagName).toBe('STRONG');
    // The h1 is the frame's (phone) or the title's (desktop); the summary and each document are h2; the card
    // titles under them are h3.

    const summary = screen.getByRole('region', { name: 'Summary' });
    expect(within(summary).getByRole('heading', { level: 3, name: 'Newsletter editor' })).toBeTruthy();
    expect(within(summary).getByText('New agent')).toBeTruthy();
    expect(within(summary).getByText('Reports to')).toBeTruthy();
    expect(screen.getByText('Sonnet 4.6 · medium')).toBeTruthy();
    expect(screen.getAllByRole('img', { name: 'Newsletter editor' }).length).toBeGreaterThan(0);

    expect(screen.getByRole('heading', { level: 2, name: 'Proposed role' })).toBeTruthy();
    expect(screen.getByRole('heading', { level: 2, name: 'Initial memory' })).toBeTruthy();
    expect(screen.getByText('role.md · opening and 2 sections')).toBeTruthy();
    // A new document has no removed or added marks: nothing to diff against.
    expect(screen.queryByText('−')).toBeNull();
    expect(screen.queryByText('+')).toBeNull();
    expect(section('Opening').open).toBe(true);
    expect(section('What you own').open).toBe(false);
    expect(screen.getByRole('button', { name: 'Approve hire' })).toBeTruthy();
  });

  it('shows a denied role update with its resolution, its diff and no decision', async () => {
    await renderProposal(goRoleUpdate);
    expect(screen.getByText('Buyer keeps its current role')).toBeTruthy();
    expect(screen.getByText('Denied')).toBeTruthy();
    expect(screen.queryByText('Needs your approval')).toBeNull();
    expect(screen.queryByRole('button', { name: /approve/i })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Deny' })).toBeNull();
    expect(screen.queryByLabelText('Note to Chief of Staff (optional)')).toBeNull();
    expect(screen.getByRole('link', { name: 'Back to Home' })).toBeTruthy();

    // The attachment the proposer sent.
    expect(screen.getByRole('button', { name: /quotes\.csv · 2 KB/ })).toBeTruthy();

    // Changed sections are open, unchanged ones are closed and say so.
    expect(section('Scope').open).toBe(true);
    expect(section('Opening').open).toBe(false);
    expect(within(section('Opening')).getByText('No changes')).toBeTruthy();
    expect(within(section('Scope')).getByText('1 added')).toBeTruthy();
    expect(within(section('Scope')).getByText('Owns vendor quotes.')).toBeTruthy();
    expect(within(section('Scope')).getByText('+')).toBeTruthy();
  });

  it('shows removed lines with a minus mark and a changed section beside unchanged ones (handbook)', async () => {
    await renderProposal(handbook);
    expect(screen.getByText('Handbook update')).toBeTruthy();
    expect(screen.getByText('Applies to')).toBeTruthy();
    expect(screen.getByText('All 6 agents')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Approve update' })).toBeTruthy();

    const money = section('Money');
    expect(money.open).toBe(true);
    expect(within(money).getByText('1 added · 1 removed')).toBeTruthy();
    expect(within(money).getByText('Managers approve spend above $5,000.')).toBeTruthy();
    expect(within(money).getByText('−')).toBeTruthy();
    expect(within(money).getByText('+')).toBeTruthy();
    expect(section('Writing').open).toBe(false);
    expect(within(section('Writing')).getByText('No changes')).toBeTruthy();
    expect(section('Opening').open).toBe(false);
  });

  it('lists a reorg as moves, marks the one that failed, and links onward from the resolution', async () => {
    const { router } = await renderProposal(goReorg);
    const moves = screen.getByRole('list', { name: 'Reporting line changes' });
    const rows = within(moves).getAllByRole('listitem');
    expect(rows).toHaveLength(2);
    // The first move lands: it has no "from" (already there), so only the target shows.
    expect(within(rows[0]!).getByText('Buyer')).toBeTruthy();
    expect(within(rows[0]!).getByText('Engineering lead')).toBeTruthy();
    // The second could not be made.
    expect(within(rows[1]!).getByText('Chief of Staff')).toBeTruthy();
    expect(within(rows[1]!).getByText('Not made: tester reports to buyer, which would make a cycle')).toBeTruthy();
    expect(screen.getByText('2 reporting lines')).toBeTruthy();

    expect(screen.getByText('1 reporting line changed and 1 could not be')).toBeTruthy();
    expect(screen.getByText('Approved')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Open Team' }));
    await flush();
    expect(router.state.location.pathname).toBe('/team');
  });

  it('gives an offboard the danger button and its one sentence, with no second dialog', async () => {
    await renderProposal(goOffboard);
    const approve = screen.getByRole('button', { name: 'Offboard tester' });
    expect(approve.className).toContain('kv-btn--danger');
    // The canvas says it in an info Banner between the summary and the decision.
    expect(screen.getByText('Nothing is deleted')).toBeTruthy();
    expect(screen.getByText('Its files move to the archive. Nothing is deleted, and you can restore it later.')).toBeTruthy();
    expect(screen.getByText('Reports to')).toBeTruthy();
    formMock.mockResolvedValue({ result: 'tester is offboarded', link: '/team' } satisfies NeedActionResponse);
    fireEvent.click(approve);
    await flush();
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(formMock).toHaveBeenCalledTimes(1);
    expect(screen.getByText('tester is offboarded')).toBeTruthy();
  });

  it('keeps the primary button for every other kind', async () => {
    await renderProposal(hire);
    expect(screen.getByRole('button', { name: 'Approve hire' }).className).toContain('kv-btn--primary');
    expect(screen.queryByText(/Its files move to the archive/)).toBeNull();
  });

  it('approves with the path, the note and the attachments as one multipart form, then shows the result', async () => {
    const quote = new File(['x'], 'quote.pdf', { type: 'application/pdf' });
    pickMock.mockResolvedValue([quote]);
    formMock.mockResolvedValue({ result: 'Newsletter editor is hired and reports to Support lead', link: '/agents/newsletter-editor' } satisfies NeedActionResponse);
    const { router } = await renderProposal(hire);

    fireEvent.change(screen.getByLabelText('Note to Chief of Staff (optional)'), { target: { value: 'Go ahead.' } });
    fireEvent.click(screen.getByRole('button', { name: 'Attach' }));
    await flush();
    expect(screen.getByRole('button', { name: 'Remove quote.pdf' })).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'Approve hire' }));
    await flush();

    expect(formMock).toHaveBeenCalledTimes(1);
    const [url, form] = formMock.mock.calls[0]!;
    expect(url).toBe('/api/v1/needs/approve');
    expect(form.get('path')).toBe(hire.path);
    expect(form.get('message')).toBe('Go ahead.');
    expect(form.getAll('attachments[]')).toEqual([quote]);

    expect(screen.getByText('Newsletter editor is hired and reports to Support lead')).toBeTruthy();
    expect(screen.getByText('Approved')).toBeTruthy();
    expect(screen.queryByText('Needs your approval')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Approve hire' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Deny' })).toBeNull();
    expect(screen.queryByLabelText('Note to Chief of Staff (optional)')).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: 'Open Newsletter editor' }));
    await flush();
    expect(router.state.location.pathname).toBe('/agents/newsletter-editor');
  });

  it('lets you take a picked file back out before sending', async () => {
    const a = new File(['a'], 'a.pdf');
    const b = new File(['b'], 'b.csv');
    pickMock.mockResolvedValue([a, b]);
    formMock.mockResolvedValue({ result: 'Approved' } satisfies NeedActionResponse);
    await renderProposal(hire);
    expect(screen.getByRole('button', { name: 'Attach' }).className).toContain('kv-btn--ghost');
    fireEvent.click(screen.getByRole('button', { name: 'Attach' }));
    await flush();
    fireEvent.click(screen.getByRole('button', { name: 'Remove a.pdf' }));
    expect(screen.queryByRole('button', { name: 'Remove a.pdf' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Approve hire' }));
    await flush();
    expect(formMock.mock.calls[0]![1].getAll('attachments[]')).toEqual([b]);
  });

  it('holds every control while the answer is posting', async () => {
    pickMock.mockResolvedValue([new File(['a'], 'a.pdf')]);
    formMock.mockReturnValue(new Promise(() => {}));
    await renderProposal(hire);
    fireEvent.click(screen.getByRole('button', { name: 'Attach' }));
    await flush();
    fireEvent.click(screen.getByRole('button', { name: 'Approve hire' }));
    await flush();
    expect((screen.getByRole('button', { name: 'Approve hire' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByRole('button', { name: 'Approve hire' }).getAttribute('aria-busy')).toBe('true');
    expect((screen.getByRole('button', { name: 'Deny' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Attach' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Remove a.pdf' }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: 'Deny' }));
    expect(formMock).toHaveBeenCalledTimes(1);
  });

  it.each([
    [409, 'hire provision: an agent with that name already exists', 'you'],
    [403, 'request refused because it came from another site', "you, from Kivali's own address"],
  ])('says what happened and who can fix it on a %i, and keeps the decision', async (status, msg, who) => {
    formMock.mockRejectedValue(new ApiError(status, msg, who));
    await renderProposal(hire);
    fireEvent.click(screen.getByRole('button', { name: 'Approve hire' }));
    await flush();
    expect(screen.getByText(msg)).toBeTruthy();
    expect(screen.getByText(who)).toBeTruthy();
    expect(screen.getByText('Needs your approval')).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Approve hire' }) as HTMLButtonElement).disabled).toBe(false);
    // Only a 404 reloads the proposal.
    expect(getMock).toHaveBeenCalledTimes(1);
  });

  it('titles the handbook summary card and says who it applies to', async () => {
    await renderProposal(handbook);
    const summary = screen.getByRole('region', { name: 'Summary' });
    expect(within(summary).getByRole('heading', { level: 3, name: 'Handbook' })).toBeTruthy();
    expect(within(summary).getByText('Applies to every agent')).toBeTruthy();
  });

  it('denies through the same form and shows the denial', async () => {
    formMock.mockResolvedValue({ result: 'Handbook stays as it is' } satisfies NeedActionResponse);
    await renderProposal(handbook);
    fireEvent.click(screen.getByRole('button', { name: 'Deny' }));
    await flush();
    const [url, form] = formMock.mock.calls[0]!;
    expect(url).toBe('/api/v1/needs/deny');
    expect(form.get('path')).toBe(handbook.path);
    expect(form.get('message')).toBe('');
    expect(screen.getByText('Handbook stays as it is')).toBeTruthy();
    expect(screen.getByText('Denied')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Approve update' })).toBeNull();
    // A denial links nowhere: only the way back.
    expect(screen.queryByRole('button', { name: 'Read it' })).toBeNull();
    expect(screen.getByRole('link', { name: 'Back to Home' })).toBeTruthy();
  });

  it('shows the resolved state when the answer was already given elsewhere (404)', async () => {
    const already: ProposalData = { ...structuredClone(hire), resolved: { approved: true, at: '2026-09-28T10:00:00Z', result: 'Newsletter editor is hired', link: '/agents/newsletter-editor' } };
    const { router } = await renderProposal(hire);
    formMock.mockRejectedValue(new ApiError(404, 'That request is already answered.', 'Nobody needs to act.'));
    getMock.mockResolvedValue(already);
    fireEvent.click(screen.getByRole('button', { name: 'Approve hire' }));
    await flush();
    expect(screen.getByText('Newsletter editor is hired')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Approve hire' })).toBeNull();
    expect(router.state.location.pathname).toBe('/proposals/' + hire.path);
  });

  it('keeps the decision and says what happened when the answer fails', async () => {
    formMock.mockRejectedValue(new ApiError(500, 'Kivali could not record your answer.', 'Try again; if it keeps happening, whoever runs this server can look into it.'));
    await renderProposal(hire);
    fireEvent.click(screen.getByRole('button', { name: 'Approve hire' }));
    await flush();
    expect(screen.getByText('Kivali could not record your answer.')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Approve hire' })).toBeTruthy();
  });

  it('says the proposal is not here on a 404, with a way home', async () => {
    getMock.mockRejectedValue(new ApiError(404, 'no such request', 'Nobody'));
    const router = createMemoryRouter(
      [
        { path: '/', element: <p>home page</p> },
        { path: '/proposals/*', element: <Proposal now={NOW} /> },
      ],
      { basename: '/', initialEntries: ['/proposals/messages/nope.md'] },
    );
    render(<RouterProvider router={router} />);
    await flush();
    expect(screen.getByText("This proposal isn't here")).toBeTruthy();
    fireEvent.click(screen.getByRole('link', { name: 'Back to Home' }));
    await flush();
    expect(router.state.location.pathname).toBe('/');
  });

  it('reports a load failure with who can fix it', async () => {
    getMock.mockRejectedValue(new ApiError(0, 'Kivali could not reach the server.', 'Check your connection.'));
    const router = createMemoryRouter([{ path: '/proposals/*', element: <Proposal now={NOW} /> }], { basename: '/', initialEntries: ['/proposals/messages/x.md'] });
    render(<RouterProvider router={router} />);
    await flush();
    expect(screen.getByText('Kivali could not reach the server.')).toBeTruthy();
    expect(screen.getByText('Check your connection.')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeTruthy();
  });

  it('shows a loading state before the proposal arrives', async () => {
    getMock.mockReturnValue(new Promise(() => {}));
    const router = createMemoryRouter([{ path: '/proposals/*', element: <Proposal now={NOW} /> }], { basename: '/', initialEntries: ['/proposals/messages/x.md'] });
    render(<RouterProvider router={router} />);
    await flush();
    expect(screen.getByLabelText('Loading proposal').getAttribute('aria-busy')).toBe('true');
  });

  it('asks the frame for the proposal chrome: 760 column, no tab bar, Home as the way back (phone shows the frame link, not its own)', async () => {
    const seen: (FrameChrome | null)[] = [];
    await renderProposal(hire, (c) => seen.push(c));
    const last = seen.filter((c): c is FrameChrome => c !== null).at(-1);
    expect(last).toEqual({ title: 'Hire a newsletter editor', tabBar: false, width: 'transcript', back: { to: '/', label: 'Home' } });
    // The page itself carries no back link; the frame draws one on desktop and in the phone header.
    expect(screen.queryByRole('button', { name: 'Home' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Home' })).toBeNull();
  });

  it('requests the proposal by its path, one encoded segment at a time', async () => {
    await renderProposal(hire);
    expect(getMock.mock.calls[0]![0]).toBe('/api/v1/proposals/messages/2026-09-28/0002-chief-of-staff.md');
  });
});
