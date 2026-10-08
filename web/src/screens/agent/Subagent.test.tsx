import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { SubagentTranscript, TranscriptRow } from '../../api/types.gen';
import { NOW, backgroundEmpty, subagentTranscript } from './tabsFixtures';
import { SLUG, renderTabs } from './tabsHarness';

const ID = 'bbbb2222';
const PATH = '/agents/' + SLUG + '/subagents/' + ID;
const API = '/api/v1/agents/' + SLUG + '/subagents/' + ID;
const STARTED = subagentTranscript.meta.started;

/** The page's own content: the frame's tree and phone header carry names and states of their own. */
const page = () => within(screen.getByRole('main'));

afterEach(() => {
  vi.unstubAllGlobals();
});

const [dispatch, ...rest] = subagentTranscript.rows;
const firstRows: TranscriptRow[] = dispatch ? [dispatch] : [];

function running(rows: TranscriptRow[]): SubagentTranscript {
  const { ended: _ended, error: _error, ...meta } = subagentTranscript.meta;
  // Started 50 s before the page's clock.
  return { meta: { ...meta, state: 'running', started: NOW - 50_000 }, rows };
}

describe('the task page: its header', () => {
  it('names the parent agent and the step, with the state, the model badge and the time it took', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: subagentTranscript } });
    expect(await page().findByRole('heading', { name: 'Check the build logs' })).toBeInTheDocument();
    expect(screen.getByText('Background task for Engineering lead · Read the logs')).toBeInTheDocument();
    expect(page().getByRole('status')).toHaveTextContent('Needs help');
    expect(screen.getByText('Haiku 4.5 · medium')).toBeInTheDocument();
    // Started 1788255000000, ended 60 s later: a finished task shows how long it took.
    expect(screen.getByText('1m')).toBeInTheDocument();
    expect(screen.getByText('The agent pod went away')).toBeInTheDocument();
  });

  it('writes a numeric step as "step 3"', async () => {
    const t: SubagentTranscript = { ...subagentTranscript, meta: { ...subagentTranscript.meta, step: '3' } };
    renderTabs(PATH, { routes: { ['GET ' + API]: t } });
    expect(await screen.findByText('Background task for Engineering lead · step 3')).toBeInTheDocument();
  });

  it('hides the tab bar and goes back to the Background tab', async () => {
    const user = userEvent.setup();
    renderTabs(PATH, { routes: { ['GET ' + API]: subagentTranscript, ['GET /api/v1/agents/' + SLUG + '/background']: backgroundEmpty } });
    await page().findByRole('heading', { name: 'Check the build logs' });
    expect(screen.queryByRole('navigation', { name: 'Tab bar' })).toBeNull();
    await user.click(page().getByRole('button', { name: 'Engineering lead · Background' }));
    expect(await screen.findByText('Nothing running in the background')).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: /Background/ })).toHaveAttribute('aria-selected', 'true');
  });

  it('draws the transcript read only, the dispatching agent as the sender', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: subagentTranscript } });
    const list = await screen.findByRole('list', { name: 'Chat with Check the build logs' });
    expect(within(list).getByText('Check the build logs.')).toBeInTheDocument();
    expect(within(list).getByText('Two flaky tests.')).toBeInTheDocument();
    expect(screen.queryByRole('textbox')).toBeNull();
  });

  it('says so when the task cannot be read', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: () => new Response(JSON.stringify({ error: 'No such task.', who: 'Whoever sent you the link.' }), { status: 404 }) } });
    expect(await screen.findByText('No such task.')).toBeInTheDocument();
  });
});

describe('the task page: following a running task', () => {
  it('opens no stream for a task that has finished', async () => {
    const h = renderTabs(PATH, { routes: { ['GET ' + API]: subagentTranscript } });
    await page().findByRole('heading', { name: 'Check the build logs' });
    expect(h.streams).toHaveLength(0);
  });

  it('tails the stream while it runs: each row it announces is appended, the count of file rows is what it resumes from', async () => {
    let body = running(firstRows);
    const h = renderTabs(PATH, { routes: { ['GET ' + API]: () => body } });
    await screen.findByText('Check the build logs.');
    expect(page().getByRole('status')).toHaveTextContent('Working');
    expect(screen.getByText('0:50')).toBeInTheDocument();
    // The stream opens at the top of the file: the page holds no count of file rows to skip.
    expect(h.streamUrls).toEqual(['/agents/' + SLUG + '/subagents/' + ID + '/stream']);
    expect(screen.queryByText('Two flaky tests.')).toBeNull();

    body = running(subagentTranscript.rows);
    const tail = h.streams[0];
    expect(tail?.started).toBe(true);
    act(() => tail?.emit('chat_message', { role: 'sent', content: 'Check the build logs.', ts: STARTED }));
    act(() => tail?.emit('chat_message', { role: 'received', content: 'Two flaky tests.', ts: STARTED + 50_000 }));
    expect(await screen.findByText('Two flaky tests.')).toBeInTheDocument();
    // Two rows announced while one read was out: at most one trailing read followed.
    expect(h.count('GET', API)).toBeLessThanOrEqual(3);
    expect(tail?.stopped).toBe(false);
  });

  it('stops tailing when the stream says done and the task has finished', async () => {
    let body = running(firstRows);
    const h = renderTabs(PATH, { routes: { ['GET ' + API]: () => body } });
    await screen.findByText('Check the build logs.');
    const tail = h.streams[0];
    body = { ...subagentTranscript, meta: { ...subagentTranscript.meta, state: 'done' } };
    body = { meta: { ...body.meta }, rows: [...firstRows, ...rest] };
    act(() => tail?.emit('meta', { status: 'completed' }));
    act(() => tail?.emit('done', {}));
    await waitFor(() => expect(tail?.stopped).toBe(true));
    expect(await screen.findByText('Two flaky tests.')).toBeInTheDocument();
    expect(page().getByRole('status')).toHaveTextContent('Done');
    const reads = h.count('GET', API);
    expect(h.streams).toHaveLength(1);
    // Nothing more is read once it has stopped.
    expect(h.count('GET', API)).toBe(reads);
  });

  it('lets the stream go while the tab is hidden and reopens it from the file rows received', async () => {
    const setVisibility = (v: DocumentVisibilityState) => {
      Object.defineProperty(document, 'visibilityState', { configurable: true, value: v });
      act(() => void document.dispatchEvent(new Event('visibilitychange')));
    };
    try {
      let body = running(firstRows);
      const h = renderTabs(PATH, { routes: { ['GET ' + API]: () => body } });
      await screen.findByText('Check the build logs.');
      const first = h.streams[0];
      // The server ids rows by file position: rows 1 and 2 (row 2 arriving twice, as a browser-level
      // reconnect can deliver it), then row 3 appended. The page draws two rows and saw four events; the
      // resume point is 3, the file's count.
      act(() => first?.emit('chat_message', { role: 'sent', content: 'Check the build logs.' }, 1));
      act(() => first?.emit('chat_message', { role: 'tool', content: '' }, 2));
      act(() => first?.emit('chat_message', { role: 'tool', content: '' }, 2));
      body = running(subagentTranscript.rows);
      act(() => first?.emit('chat_message', { role: 'received', content: 'Two flaky tests.' }, 3));
      expect(await screen.findByText('Two flaky tests.')).toBeInTheDocument();

      setVisibility('hidden');
      expect(first?.stopped).toBe(true);
      expect(h.streams).toHaveLength(1);
      const reads = h.count('GET', API);

      setVisibility('visible');
      expect(h.streams).toHaveLength(2);
      expect(h.streamUrls[1]).toBe('/agents/' + SLUG + '/subagents/' + ID + '/stream?from=3');
      expect(h.streams[1]?.started).toBe(true);
      // Coming back reads the transcript at once rather than waiting for the next row.
      await waitFor(() => expect(h.count('GET', API)).toBe(reads + 1));
    } finally {
      Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    }
  });

  it('lets the stream go when the page closes', async () => {
    const h = renderTabs(PATH, { routes: { ['GET ' + API]: running(firstRows), ['GET /api/v1/agents/' + SLUG + '/background']: backgroundEmpty } });
    await screen.findByText('Check the build logs.');
    const user = userEvent.setup();
    await user.click(page().getByRole('button', { name: 'Engineering lead · Background' }));
    await screen.findByText('Nothing running in the background');
    expect(h.streams[0]?.stopped).toBe(true);
  });

  it('stops tailing when the task ends in an error', async () => {
    let body = running(firstRows);
    const h = renderTabs(PATH, { routes: { ['GET ' + API]: () => body } });
    await screen.findByText('Check the build logs.');
    const tail = h.streams[0];
    body = subagentTranscript;
    act(() => tail?.emit('meta', { status: 'errored' }));
    await waitFor(() => expect(tail?.stopped).toBe(true));
    expect(page().getByRole('status')).toHaveTextContent('Needs help');
    expect(screen.getByText('The agent pod went away')).toBeInTheDocument();
  });
});
