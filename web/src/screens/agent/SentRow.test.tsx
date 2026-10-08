import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ReactNode } from 'react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { describe, expect, it } from 'vitest';
import { goChat } from '../../state/fixtures/transcript';
import type { ChatRow } from '../../state/transcript';
import { SentRow } from './SentRow';
import { Transcript, keyRows } from './Transcript';

function renderAt(node: ReactNode) {
  const router = createMemoryRouter([{ path: '*', element: node }], { basename: '/', initialEntries: ['/x'] });
  render(<RouterProvider router={router} />);
}

// The golden doc_published row (chat.json): a notice to Buyer and tester with a body, a file and assignment #42.
const golden = goChat.rows.find((r) => r.kind === 'doc_published');
if (!golden) throw new Error('chat.json has no doc_published row');
const sent: ChatRow = { ...golden, key: 'd:' + golden.tool_use_id };

const LINE = 'Sent “Quote accepted” to Buyer, tester';

describe('a message the agent sent (SentRow)', () => {
  it('starts as one collapsed line naming the message, its recipients and its type', () => {
    renderAt(<SentRow row={sent} />);
    const line = screen.getByRole('button', { name: new RegExp(LINE) });
    expect(line).toHaveAttribute('aria-expanded', 'false');
    expect(within(line).getByText('notice')).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Quote accepted' })).toBeNull();
    expect(screen.queryByText('$3.8k')).toBeNull();
  });

  it('opens in place to the message as a card: body, recipients, attachments, assignment and the raw file', async () => {
    renderAt(<SentRow row={sent} />);
    const line = screen.getByRole('button', { name: new RegExp(LINE) });
    await userEvent.click(line);
    expect(line).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByRole('heading', { name: 'Quote accepted' })).toBeInTheDocument();
    expect(screen.getByText('Sent to Buyer · also to tester')).toBeInTheDocument();
    // The body is rendered markdown.
    expect(screen.getByText('$3.8k').tagName).toBe('STRONG');
    expect(screen.getByRole('button', { name: /hosting-quote\.pdf/ })).toHaveAttribute('title', 'Download hosting-quote.pdf');
    expect(screen.getByRole('link', { name: /#42/ })).toHaveAttribute('href', '/assignments/42');
    const raw = screen.getByRole('link', { name: 'Raw message' });
    expect(raw).toHaveAttribute('href', '/messages/2026-09-01/0004-engineering-lead.md');
    expect(raw).toHaveAttribute('target', '_blank');
    await userEvent.click(line);
    expect(screen.queryByRole('heading', { name: 'Quote accepted' })).toBeNull();
  });

  it('does not open when the message file could not be read: the line alone, no body', () => {
    const bare: ChatRow = { key: 'd:x', kind: 'doc_published', ts: 1, tool_use_id: 'x', title: 'Weekly notes', to: [], attachments: [], raw_url: '/messages/gone.md' };
    renderAt(<SentRow row={bare} />);
    expect(screen.getByText('Sent “Weekly notes”')).toBeInTheDocument();
    expect(screen.queryByRole('button')).toBeNull();
    expect(screen.queryByRole('link')).toBeNull();
  });

  it('has no "Open" link, and only "Raw message" points at the stored markdown file', async () => {
    renderAt(<Transcript rows={keyRows(goChat.rows)} agent={{ slug: 'engineering-lead', name: 'Engineering lead' }} now={goChat.rows[0]?.ts ?? 0} readOnly />);
    expect(screen.queryByRole('link', { name: 'Open' })).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: new RegExp(LINE) }));
    const card = screen.getByRole('heading', { name: 'Quote accepted' }).closest('section');
    if (!card) throw new Error('no card');
    const toFiles = within(card as HTMLElement)
      .getAllByRole('link')
      .filter((a) => /\.md$/.test(a.getAttribute('href') ?? ''));
    expect(toFiles.map((a) => a.textContent)).toEqual(['Raw message']);
  });
});
