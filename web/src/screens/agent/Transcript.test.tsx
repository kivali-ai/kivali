import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { type ReactNode, useState } from 'react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { describe, expect, it, vi } from 'vitest';
import type { PastChatDetail, SubagentTranscript, TranscriptRow } from '../../api/types.gen';
import { identityColorFor } from '../../lib/agentIdentity';
import { goChat, goChatMessageTaskResult, goPastChatDetail, goSubagentTranscript, goToolCallDetail } from '../../state/fixtures/transcript';
import { Transcript, keyRows } from './Transcript';
import { DRAFT_TTL_MS, loadDraft, saveDraft } from './drafts';
import { duration, fileSize, messageTime, modelBadge } from './format';

vi.mock('../../state/OrgProvider', async () => {
  const { identifierFor } = await import('../../lib/agentIdentity');
  return { usePersonName: () => 'Maya Chen', useAgentIdentify: () => identifierFor([]) };
});

function renderAt(node: ReactNode) {
  const router = createMemoryRouter([{ path: '*', element: node }], { basename: '/', initialEntries: ['/x'] });
  render(<RouterProvider router={router} />);
}

describe('Transcript, reused read only', () => {
  it('draws a past chat with no pending messages or actions (past_chat_detail.json)', () => {
    const detail = goPastChatDetail as PastChatDetail;
    renderAt(<Transcript rows={keyRows(detail.rows)} agent={{ slug: 'engineering-lead', name: 'Engineering lead' }} now={detail.summary.range.to} readOnly />);
    const list = screen.getByRole('list', { name: 'Chat with Engineering lead' });
    expect(within(list).queryByRole('button', { name: 'Send now' })).toBeNull();
    expect(within(list).getAllByRole('listitem').length).toBeGreaterThan(0);
  });

  it('draws a background task with the dispatching agent as the sender (subagent_transcript.json)', () => {
    const t = goSubagentTranscript as SubagentTranscript;
    renderAt(<Transcript rows={keyRows(t.rows)} agent={{ slug: t.meta.id, name: t.meta.description }} senderSlug={t.meta.parent} now={t.meta.started} readOnly />);
    expect(screen.getByText('Check the build logs.')).toBeInTheDocument();
    expect(screen.getByText('Two flaky tests.')).toBeInTheDocument();
    expect(screen.getByText('haiku 4.5 · medium')).toBeInTheDocument();
    // Both parties are agents: neither is drawn with a person's avatar.
    expect(screen.getByRole('img', { name: 'Engineering lead' })).toHaveClass('kv-avatar--agent');
    // Coloured by its slug, as the sidebar colours it, never by its name.
    expect(screen.getByRole('img', { name: 'Engineering lead' }).style.background).toBe('var(--identity-' + identityColorFor(t.meta.parent) + ')');
  });
});

describe('a tool call the chat API cut (chat.json, tool_call_detail.json)', () => {
  const cut = goChat.rows.find((r) => r.tool_use_id === 'toolu_06');
  if (!cut) throw new Error('chat.json has no cut tool row');
  const agent = { slug: 'engineering-lead', name: 'Engineering lead' };

  it('opens on the cut payload and loads the full call from the tool-calls endpoint', async () => {
    const fetchFn = vi.fn((_url: string) => Promise.resolve(new Response(JSON.stringify(goToolCallDetail), { status: 200 })));
    vi.stubGlobal('fetch', fetchFn);
    try {
      renderAt(<Transcript rows={keyRows([cut])} agent={agent} now={cut.ts} />);
      await userEvent.click(screen.getByText('file_create'));
      expect(screen.getByText(/Wrote notes\/plan\.md \(48213 bytes\)…/)).toBeInTheDocument();
      await userEvent.click(screen.getByRole('button', { name: 'Show the full call' }));
      expect(await screen.findByText('Wrote notes/plan.md (48213 bytes) and indexed it.')).toBeInTheDocument();
      expect(screen.getByText(/The whole file\./)).toBeInTheDocument();
      expect(fetchFn).toHaveBeenCalledTimes(1);
      expect(fetchFn.mock.calls[0]?.[0]).toBe('/api/v1/agents/engineering-lead/tool-calls/toolu_06');
      expect(screen.queryByRole('button', { name: 'Show the full call' })).toBeNull();
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('says why when the call is gone, and keeps the button to try again', async () => {
    const gone = { error: "this tool call is no longer in engineering-lead's current chat", who: 'you: reload the page to see the chat as it is now' };
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(JSON.stringify(gone), { status: 404 }))));
    try {
      renderAt(<Transcript rows={keyRows([cut])} agent={agent} now={cut.ts} />);
      await userEvent.click(screen.getByText('file_create'));
      await userEvent.click(screen.getByRole('button', { name: 'Show the full call' }));
      expect(await screen.findByText(gone.error)).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Show the full call' })).toBeEnabled();
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('offers the full call again when a cut output lands after it was loaded mid-call', async () => {
    const running: TranscriptRow = { ...cut, status: 'running', output_truncated: false };
    delete running.output;
    delete running.ended_ts;
    let setRows: (rows: TranscriptRow[]) => void = () => {};
    function Live() {
      const [rows, set] = useState<TranscriptRow[]>([running]);
      setRows = set;
      return <Transcript rows={keyRows(rows)} agent={agent} now={running.ts} />;
    }
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(JSON.stringify({ ...goToolCallDetail, output: '' }), { status: 200 }))));
    try {
      renderAt(<Live />);
      await userEvent.click(screen.getByText('file_create'));
      await userEvent.click(screen.getByRole('button', { name: 'Show the full call' }));
      await waitFor(() => expect(screen.queryByRole('button', { name: 'Show the full call' })).toBeNull());
      act(() => setRows([cut]));
      expect(screen.getByRole('button', { name: 'Show the full call' })).toBeInTheDocument();
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('offers nothing on a call that was not cut', () => {
    const whole = goChat.rows.find((r) => r.tool_use_id === 'toolu_01');
    if (!whole) throw new Error('chat.json has no toolu_01');
    renderAt(<Transcript rows={keyRows([whole])} agent={agent} now={whole.ts} />);
    expect(screen.queryByRole('button', { name: 'Show the full call' })).toBeNull();
  });
});

describe('you, in a transcript', () => {
  it('are drawn under your own name and initials, as on the org chart, not the row\u2019s "You"', () => {
    const you = { kind: 'person' as const, slug: 'ceo', name: 'You' };
    const rows = keyRows([
      { kind: 'message', ts: 1, role: 'sent', from: you, body_md: 'Check the build logs.' },
      { kind: 'delivery', ts: 2, role: 'received', from: you, title: 'A note', body_md: 'From the inbox.' },
    ]);
    renderAt(<Transcript rows={rows} agent={{ slug: 'a', name: 'A' }} now={2} readOnly />);
    expect(screen.queryByText('You')).toBeNull();
    expect(screen.getAllByText('Maya Chen')).toHaveLength(2);
    expect(screen.getAllByText('MC')).toHaveLength(2);
  });
});

describe('a background task’s result', () => {
  it('is drawn where it landed, saying the task finished, closed on what it answered (chat_message_task_result.json)', () => {
    const row = goChatMessageTaskResult.row;
    if (!row) throw new Error('the golden event carries no row');
    const rows = keyRows([
      { kind: 'message', ts: row.ts - 1000, role: 'received', from: { kind: 'agent', slug: 'engineering-lead', name: 'Engineering lead' }, body_md: 'I have asked for a comparison.', attachments: [], pending: false },
      row,
      { kind: 'message', ts: row.ts + 1000, role: 'received', from: { kind: 'agent', slug: 'engineering-lead', name: 'Engineering lead' }, body_md: 'The comparison is back.', attachments: [], pending: false },
    ]);
    renderAt(<Transcript rows={rows} agent={{ slug: 'engineering-lead', name: 'Engineering lead' }} now={row.ts + 2000} />);
    const items = within(screen.getByRole('list', { name: 'Chat with Engineering lead' })).getAllByRole('listitem');
    // Between the reply before it and the reply it woke the agent for.
    expect(items.map((li) => within(li).queryByText(/comparison|compare/i) !== null)).toEqual([true, true, true]);
    const result = items[1] as HTMLElement;
    // The sender's line says what happened; the task's own line names it.
    expect(within(result).getByText('Background task finished')).toBeInTheDocument();
    expect(within(result).getByRole('img', { name: 'Background task finished' })).toHaveClass('kv-avatar--agent');
    expect(within(result).getByText('Compare hosting providers')).toBeVisible();
    expect(within(result).getByText('sonnet 4.6 · low')).toBeVisible();
    // Closed until asked for: the answer and the way to the session are behind the task's line.
    expect(result.querySelector('details')).not.toHaveAttribute('open');
    expect(within(result).getByText('Three providers quote under $4k.')).not.toBeVisible();
    expect(within(result).getByRole('link', { name: /Open full session/, hidden: true })).toHaveAttribute('href', '/agents/engineering-lead/subagents/aaaa1111');
  });

  it('says a failed task failed, under the same avatar as one that finished', () => {
    const task = { index: 0, id: 'bbbb2222', title: 'Check the build logs', model: 'Haiku 4.5', effort: 'medium', url: '/api/v1/agents/a/subagents/bbbb2222' };
    const rows = keyRows([
      { kind: 'task_result', ts: 1, tasks: [{ ...task, state: 'done', output_md: 'Two flaky tests.' }] },
      { kind: 'task_result', ts: 2, tasks: [{ ...task, state: 'errored', error: 'The agent pod went away' }] },
    ]);
    renderAt(<Transcript rows={rows} agent={{ slug: 'a', name: 'A' }} now={2} />);
    const finished = screen.getByRole('img', { name: 'Background task finished' });
    const failed = screen.getByRole('img', { name: 'Background task failed' });
    expect(screen.getByText('Background task failed')).toBeInTheDocument();
    expect(screen.getByText('Failed')).toBeVisible();
    expect(screen.getByText('The agent pod went away')).not.toBeVisible();
    // The colour would follow the name, and the two names differ.
    expect(finished.style.background).toBe('var(--identity-slate)');
    expect(failed.style.background).toBe('var(--identity-slate)');
    expect(failed).toHaveTextContent('BT');
  });

  it('holds the text of a report that names no task, closed', () => {
    renderAt(<Transcript rows={keyRows([{ kind: 'task_result', ts: 1, tasks: [], body_md: 'A report that names no task.' }])} agent={{ slug: 'a', name: 'A' }} now={1} />);
    expect(screen.getByText('Background task')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Report' })).toBeVisible();
    expect(screen.getByText('A report that names no task.')).not.toBeVisible();
  });
});

describe('attachments (chat-attachment-markdown)', () => {
  it('opens markdown rendered in a new tab and downloads anything else in place', async () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const rows = keyRows([
      {
        kind: 'message',
        ts: 1,
        role: 'sent',
        from: { kind: 'person', slug: 'ceo', name: 'You' },
        body_md: 'Two files',
        attachments: [
          { name: 'notes.md', size_bytes: 2048, url: '/attachments/aa' },
          { name: 'quote.pdf', size_bytes: 0, url: '/attachments/bb' },
        ],
      },
    ]);
    renderAt(<Transcript rows={rows} agent={{ slug: 'a', name: 'A' }} now={1} />);
    await userEvent.click(screen.getByRole('button', { name: 'notes.md · 2 KB' }));
    expect(open).toHaveBeenCalledWith('/attachments/aa', '_blank', 'noopener');
    expect(screen.getByRole('button', { name: 'quote.pdf' })).toHaveAttribute('title', 'Download quote.pdf');
    open.mockRestore();
  });
});

describe('format', () => {
  const now = new Date(2026, 8, 28, 14, 30).getTime();
  it('writes times as the canvas does', () => {
    expect(messageTime(new Date(2026, 8, 28, 9, 2).getTime(), now)).toBe('09:02');
    expect(messageTime(new Date(2026, 8, 24, 9, 14).getTime(), now)).toBe('Thu 09:14');
    expect(messageTime(new Date(2026, 8, 12, 9, 14).getTime(), now)).toBe('12 Sept 09:14');
    expect(messageTime(0, now)).toBe('');
  });
  it('writes badges, sizes and durations', () => {
    // The mono badge form ("opus · high"), keeping the version so a pinned model reads apart.
    expect(modelBadge('Opus 5.5', 'high')).toBe('opus 5.5 · high');
    expect(modelBadge('Sonnet 4.6')).toBe('sonnet 4.6');
    expect(modelBadge(undefined, 'high')).toBe('');
    expect(fileSize(86016)).toBe('84 KB');
    expect(fileSize(0)).toBe('');
    expect(duration(1000, 1300)).toBe('0.3s');
    expect(duration(0, 108_000)).toBe('');
    expect(duration(1, 108_001)).toBe('1m 48s');
  });
});

describe('drafts', () => {
  it('saves, loads, clears on empty and drops a draft older than a week', () => {
    window.localStorage.clear();
    saveDraft('a', 'hello', 1000);
    expect(loadDraft('a', 2000)).toBe('hello');
    expect(loadDraft('a', 1000 + DRAFT_TTL_MS + 1)).toBe('');
    expect(window.localStorage.getItem('kivali.draft.a')).toBeNull();
    saveDraft('b', 'x', 1);
    saveDraft('b', '  ', 2);
    expect(window.localStorage.getItem('kivali.draft.b')).toBeNull();
  });
});
