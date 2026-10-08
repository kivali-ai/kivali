import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { goAgentDetail } from '../../state/fixtures/transcript';
import { pastChat, pastChatDetail, pastChats } from './tabsFixtures';
import { SLUG, renderTabs } from './tabsHarness';

const LIST = '/api/v1/agents/' + SLUG + '/chats';
const ONE = (ts: string) => LIST + '/' + ts;
const page = () => within(screen.getByRole('main'));

afterEach(() => {
  vi.unstubAllGlobals();
});

const middle = pastChat(
  'c2',
  {
    digest_md: 'Fixed **six** login bugs; one needed a second pass.',
    memory_added: ['Build 0.9.4 fails on about one run in six [[ep:20260902T093000.000000000Z]]', 'Ren replies within the hour'],
    habits_diff: { before: '- Ask for two quotes.\n- Read results first.', after: '- Ask for three quotes.\n- Read results first.' },
  },
  { prev_ts: 'c1', next_ts: 'c3' },
);

const routes = {
  ['GET ' + LIST]: pastChats,
  ['GET ' + ONE('c1')]: pastChat('c1', {}, { next_ts: 'c2' }),
  ['GET ' + ONE('c2')]: middle,
  ['GET ' + ONE('c3')]: pastChat('c3', {}, { prev_ts: 'c2' }),
};

async function opened(ts = 'c2', over = routes, detail = goAgentDetail) {
  const h = renderTabs('/agents/' + SLUG + '/chats/' + ts, { routes: over, detail });
  await page().findByRole('heading', { level: 2, name: pastChats.chats.find((c) => c.ts === ts)?.title ?? '' });
  return h;
}

describe('a past chat opens on its summary', () => {
  it('says what happened, in prose', async () => {
    await opened();
    expect(page().getByText('What happened')).toBeInTheDocument();
    expect(page().getByText('six').tagName).toBe('STRONG');
    expect(page().getByText('2 to 10 Sept · 40 messages · ~62k tokens')).toBeInTheDocument();
  });

  it('says the summary is still being written when the digest is empty', async () => {
    await opened('c2', { ...routes, ['GET ' + ONE('c2')]: pastChat('c2', { digest_md: '' }, { prev_ts: 'c1', next_ts: 'c3' }) });
    expect(page().getByText('The summary is still being written.')).toBeInTheDocument();
  });

  it('lists the notes it learned, without the memory’s provenance markers', async () => {
    await opened();
    const notes = page().getByRole('region', { name: 'Notes it learned' });
    expect(within(notes).getByText('Build 0.9.4 fails on about one run in six')).toBeInTheDocument();
    expect(within(notes).getByText('Ren replies within the hour')).toBeInTheDocument();
    expect(within(notes).getAllByText('Learned')).toHaveLength(2);
    expect(notes).not.toHaveTextContent('[[ep:');
  });

  it('says so when nothing was learned', async () => {
    await opened('c1');
    expect(within(page().getByRole('region', { name: 'Notes it learned' })).getByText(/added nothing to its memory/)).toBeInTheDocument();
  });

  it('shows the habits before and after, changed sections open', async () => {
    await opened();
    const habits = page().getByRole('region', { name: 'Habits before and after' });
    expect(within(habits).getByText('Ask for two quotes.')).toBeInTheDocument();
    expect(within(habits).getByText('Ask for three quotes.')).toBeInTheDocument();
    expect(habits.querySelector('details')).toHaveAttribute('open');
  });

  it('keeps unchanged habits folded', async () => {
    await opened('c2', { ...routes, ['GET ' + ONE('c2')]: pastChat('c2', { habits_diff: { before: '- Same.', after: '- Same.' } }, { prev_ts: 'c1', next_ts: 'c3' }) });
    const habits = page().getByRole('region', { name: 'Habits before and after' });
    expect(within(habits).getByText('No changes')).toBeInTheDocument();
    expect(habits.querySelector('details')).not.toHaveAttribute('open');
  });

  it('draws the golden summary (past_chat_detail.json)', async () => {
    const ts = pastChatDetail.summary.ts;
    renderTabs('/agents/' + SLUG + '/chats/' + ts, { routes: { ['GET ' + LIST]: { chats: [pastChatDetail.summary] }, ['GET ' + ONE(ts)]: pastChatDetail } });
    expect(await page().findByRole('heading', { level: 2, name: 'Chose the hosting provider' })).toBeInTheDocument();
    expect(page().getByText('Compared three hosts and chose the cheapest.')).toBeInTheDocument();
    expect(page().getByText('The host gives a discount on a yearly plan')).toBeInTheDocument();
  });
});

describe('the transcript is one button away', () => {
  it('is not drawn until it is asked for, then read only', async () => {
    const user = userEvent.setup();
    const ts = pastChatDetail.summary.ts;
    renderTabs('/agents/' + SLUG + '/chats/' + ts, { routes: { ['GET ' + LIST]: { chats: [pastChatDetail.summary] }, ['GET ' + ONE(ts)]: pastChatDetail } });
    await page().findByRole('heading', { level: 2, name: 'Chose the hosting provider' });
    const toggle = page().getByRole('button', { name: 'Read the transcript' });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(page().queryByRole('list', { name: 'Chat with Engineering lead' })).toBeNull();
    await user.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    const list = page().getByRole('list', { name: 'Chat with Engineering lead' });
    expect(within(list).getByText('Where are we on the quote?')).toBeInTheDocument();
    expect(page().queryByRole('textbox')).toBeNull();
    expect(within(list).queryByRole('button', { name: 'Send now' })).toBeNull();
    await user.click(toggle);
    expect(page().queryByRole('list', { name: 'Chat with Engineering lead' })).toBeNull();
  });
});

describe('the pager under the tabs', () => {
  it('names the chat and where it sits among the agent’s chats', async () => {
    await opened();
    expect(page().getByText('Past chat · 2 to 10 Sept')).toBeInTheDocument();
    expect(page().getByText('2 of 3')).toBeInTheDocument();
    expect(page().getByRole('button', { name: 'Current chat' })).toBeInTheDocument();
  });

  it('steps to the older and the newer chat', async () => {
    const user = userEvent.setup();
    await opened();
    await user.click(page().getByRole('button', { name: 'Older chat' }));
    expect(await page().findByRole('heading', { level: 2, name: 'Release 1 post-mortem' })).toBeInTheDocument();
    expect(page().getByText('1 of 3')).toBeInTheDocument();
    // The oldest chat has nothing older.
    expect(page().getByRole('button', { name: 'Older chat' })).toBeDisabled();
    await user.click(page().getByRole('button', { name: 'Newer chat' }));
    expect(await page().findByRole('heading', { level: 2, name: 'Login fixes for the web app' })).toBeInTheDocument();
    await user.click(page().getByRole('button', { name: 'Newer chat' }));
    expect(await page().findByRole('heading', { level: 2, name: 'Hosting quotes and the order' })).toBeInTheDocument();
    expect(page().getByRole('button', { name: 'Newer chat' })).toBeDisabled();
  });

  it('jumps back to the current chat', async () => {
    const user = userEvent.setup();
    const h = await opened('c3');
    h.routes['GET /api/v1/agents/' + SLUG + '/chat'] = { rows: [], pending: [], fill: { pct: 10, tokens: 10, limit: 100, bucket: '', long_threshold: 80, resolved_model: '' }, models: [], current_model: '', current_effort: '', running: false, waiting_tasks: 0, archived: false };
    await user.click(page().getByRole('button', { name: 'Current chat' }));
    expect(await screen.findByRole('textbox', { name: 'Message Engineering lead' })).toBeInTheDocument();
  });

  it('leaves out Current chat for an archived agent', async () => {
    await opened('c2', routes, { ...goAgentDetail, archived: true });
    expect(page().queryByRole('button', { name: 'Current chat' })).toBeNull();
  });

  it('is icon only on a phone', async () => {
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: false, media: q, addEventListener: () => {}, removeEventListener: () => {} }));
    await opened();
    const older = page().getByRole('button', { name: 'Older chat' });
    expect(older).toHaveClass('kv-btn--icon');
  });

  it('is labelled on a wide screen', async () => {
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: true, media: q, addEventListener: () => {}, removeEventListener: () => {} }));
    await opened();
    expect(page().getByRole('button', { name: 'Older chat' })).not.toHaveClass('kv-btn--icon');
  });
});
