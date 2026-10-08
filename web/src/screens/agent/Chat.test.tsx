import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Chat } from '../../api/types.gen';
import { goSnapshot } from '../../state/fixtures';
import { emptyChat, freshChatAfter, goAgentDetail, goChat, goChatSettings, goPendingMessage, goPendingRestore, sequences } from '../../state/fixtures/transcript';
import { NOW, SLUG, renderAgent, reply, snapshotWith } from './chatHarness';
import type { Harness } from './chatHarness';

vi.mock('../../lib/pickFiles', () => ({
  pickFiles: vi.fn(() =>
    Promise.resolve([new File(['%PDF'], 'hosting-quote.pdf', { type: 'application/pdf', lastModified: 1 }), new File(['# Notes'], 'notes.md', { lastModified: 2 })]),
  ),
}));

const CHAT = '/api/v1/agents/' + SLUG + '/chat';
const BASE = '/api/v1/agents/' + SLUG;

const idle: Chat = { ...emptyChat, fill: { ...emptyChat.fill, pct: 40 } };

function transcript() {
  return within(screen.getByRole('list', { name: 'Chat with Engineering lead' }));
}

async function loaded() {
  await screen.findByRole('list', { name: 'Chat with Engineering lead' });
}

function composer() {
  return screen.getByRole('textbox', { name: 'Message Engineering lead' });
}

function gets(h: Harness, url: string) {
  return h.calls.filter((c) => c.method === 'GET' && c.url === url).length;
}

beforeEach(() => {
  window.localStorage.clear();
});
afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Chat: the golden transcript', () => {
  it('renders every row kind of chat.json with the design system', async () => {
    const user = userEvent.setup();
    const h = renderAgent({ chat: goChat, snapshot: snapshotWith('waiting', 82) });
    await loaded();
    const t = transcript();
    // Your message, with its attachment as a download button.
    expect(t.getByText('Where are we on the quote?')).toBeInTheDocument();
    expect(t.getAllByRole('button', { name: 'hosting-quote.pdf · 84 KB' }).length).toBeGreaterThan(0);
    // The agent's reply with its model badge, and a failed call's text.
    expect(t.getByText('Checking now.')).toBeInTheDocument();
    expect(t.getByText('opus 5.5 · high')).toBeInTheDocument();
    expect(t.getByText('API Error: usage limit')).toBeInTheDocument();
    // A task's result is a message from the task where it landed, saying it finished; one that names no
    // task holds its text.
    expect(t.getAllByText('Background task finished')).toHaveLength(1);
    expect(t.getAllByText('Background task')).toHaveLength(1);
    expect(t.getByText('A report that names no task.')).toBeInTheDocument();
    // The delivery is a card from its sender.
    expect(t.getByRole('heading', { name: 'Quote is in' })).toBeInTheDocument();
    expect(t.getByText('Delivered to Engineering lead · also to tester')).toBeInTheDocument();
    expect(t.getByText('assignment update')).toBeInTheDocument();
    expect(t.getByRole('link', { name: /#42/ })).toHaveAttribute('href', '/assignments/42');
    expect(t.getByText('Replies go to Buyer')).toBeInTheDocument();
    // What the agent sent you points at Home.
    expect(t.getByRole('heading', { name: 'Approve the hosting spend' })).toBeInTheDocument();
    expect(t.getByRole('button', { name: 'Answer on Home' })).toBeInTheDocument();
    // Tool calls, the published document, the shared file, the task batch.
    expect(t.getByText('Bash')).toBeInTheDocument();
    expect(t.getByText('WebFetch')).toBeInTheDocument();
    expect(t.getByText('Sent “Quote accepted” to Buyer, tester')).toBeInTheDocument();
    expect(t.getByText('Shared file: hosting-quote.pdf')).toBeInTheDocument();
    expect(t.getByText('3 subagents')).toBeInTheDocument();
    // Two in the batch that have an id, then the one that reported.
    expect(t.getAllByRole('link', { name: /Open full session/, hidden: true }).map((a) => a.getAttribute('href'))).toEqual([
      '/agents/engineering-lead/subagents/aaaa1111',
      '/agents/engineering-lead/subagents/bbbb2222',
      '/agents/engineering-lead/subagents/aaaa1111',
    ]);
    expect(t.getByText('The agent pod went away')).toBeInTheDocument();
    // Markers and the rotation note.
    expect(t.getByText('Chat interrupted by Stop')).toBeInTheDocument();
    expect(t.getByText('Paused to deliver your message')).toBeInTheDocument();
    expect(t.getByText('New chat requested')).toBeInTheDocument();
    // The pending message, with Delete (it is still deletable) and Send now.
    expect(t.getByText('Also check the staging deploy.')).toBeInTheDocument();
    expect(t.getByText('Pending')).toBeInTheDocument();
    expect(t.getByText('Waiting for a moment to deliver')).toBeInTheDocument();
    expect(t.getByRole('button', { name: 'Delete' })).toBeInTheDocument();
    expect(t.getByRole('button', { name: 'Send now' })).toBeInTheDocument();
    // Running with a task outstanding: the dots say so, once.
    expect(t.getAllByText(/^Waiting on 1 task/)).toHaveLength(1);
    // Wake notes are hidden until asked for.
    expect(t.queryByText('Since your last turn: #42 moved to review.')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Show wake notes (1)' }));
    expect(t.getByText('Since your last turn: #42 moved to review.')).toBeInTheDocument();
    // A running chat opens the agent stream.
    expect(h.streamUrls).toEqual(['/agents/engineering-lead/stream']);
  });

  it('never animates a call the server lists as running when no turn is (chat API note)', async () => {
    renderAgent({ chat: { ...goChat, running: false, waiting_tasks: 0 } });
    await loaded();
    expect(transcript().getByText('No result was recorded: the turn ended before this call came back.')).toBeInTheDocument();
  });
});

describe('Chat: the header (AgentPage)', () => {
  it.each([
    [40, 'success'],
    [60, 'cobalt'],
  ])('shows a quiet gauge at %i%%', async (pct, tone) => {
    renderAgent({ chat: { ...idle, fill: { ...idle.fill, pct } }, snapshot: snapshotWith('idle', pct) });
    await loaded();
    const bar = await screen.findByRole('progressbar');
    await waitFor(() => expect(bar).toHaveAttribute('aria-valuenow', String(pct)));
    expect(bar).toHaveClass('kv-progress--' + tone);
    expect(screen.getByRole('button', { name: 'New chat' })).toHaveClass('kv-btn--secondary');
  });

  it('turns into the honey badge at 85% and New chat turns primary', async () => {
    renderAgent({ chat: { ...idle, fill: { ...idle.fill, pct: 85 } }, snapshot: snapshotWith('idle', 85) });
    await loaded();
    expect(await screen.findByText('Context 85% full')).toBeInTheDocument();
    expect(screen.queryByRole('progressbar')).toBeNull();
    expect(screen.getByRole('button', { name: 'New chat' })).toHaveClass('kv-btn--primary');
  });

  it('names the agent, its role line and state, and routes the tabs', async () => {
    const user = userEvent.setup();
    // Two tasks out, as the agent's own read counts them: the server takes both numbers from one place.
    renderAgent({ chat: idle, snapshot: snapshotWith('running', 40, undefined, 2) });
    await loaded();
    // Below 960 (the default here) the phone header's h1 is the page's only one.
    expect(screen.getByRole('heading', { level: 1, name: 'Engineering lead' })).toHaveClass('app-phone-title');
    expect(screen.getByText('Owns the product from plan to release.')).toBeInTheDocument();
    expect(screen.getAllByText('Working').length).toBeGreaterThan(0);
    expect(document.title).toBe('Kivali · Engineering lead');
    const tabs = screen.getByRole('tablist');
    expect(within(tabs).getByRole('tab', { name: /Background/ })).toHaveTextContent('2');
    expect(within(tabs).getByRole('tab', { name: /Past chats/ })).toHaveTextContent('5');
    await user.click(within(tabs).getByRole('tab', { name: /About/ }));
    // The tab bodies are other screens; only the routing is this page's.
    await waitFor(() => expect(screen.getByRole('tab', { name: /About/ })).toHaveAttribute('aria-selected', 'true'));
    expect(screen.getByRole('tab', { name: /Chat/ })).toHaveAttribute('aria-selected', 'false');
  });

  it('makes the agent name the page h1 only from 960, and a plain paragraph below', async () => {
    renderAgent({ chat: idle, snapshot: snapshotWith('running') });
    await loaded();
    const name = () => screen.getByText('Engineering lead', { selector: '.app-agent-name' });
    expect(name().tagName).toBe('P');
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1);
  });

  it('makes the agent name the page h1 from 960', async () => {
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: q.includes('min-width'), media: q, addEventListener: () => {}, removeEventListener: () => {} }));
    renderAgent({ chat: idle, snapshot: snapshotWith('running') });
    await loaded();
    const name = screen.getByText('Engineering lead', { selector: '.app-agent-name' });
    expect(name.tagName).toBe('H1');
    // The phone header is hidden by CSS from 960; the page's own name is the visible h1.
    expect(screen.getAllByRole('heading', { level: 1 }).filter((h) => !h.classList.contains('app-phone-title'))).toEqual([name]);
  });

  it('gives every current tab a real panel: the trigger names it in aria-controls and it is labelled by the trigger', async () => {
    renderAgent({ chat: idle, snapshot: snapshotWith('running') });
    await loaded();
    const trigger = screen.getByRole('tab', { name: /Chat/ });
    const panel = screen.getByRole('tabpanel');
    expect(trigger.getAttribute('aria-controls')).toBe(panel.id);
    expect(panel.getAttribute('aria-labelledby')).toBe(trigger.id);
    expect(panel).toContainElement(composer());
  });

  it('confirms New chat with the verbatim dialog, and says so in words when the agent is busy (409)', async () => {
    const user = userEvent.setup();
    const h = renderAgent({ chat: idle, routes: { ['POST ' + BASE + '/new-chat']: reply(409, { error: 'busy', who: 'x' }) } });
    await loaded();
    await user.click(screen.getByRole('button', { name: 'New chat' }));
    const dialog = await screen.findByRole('dialog', { name: 'Start a new chat with Engineering lead?' });
    expect(
      within(dialog).getByText(
        'Engineering lead first folds this chat into what it remembers and its habits. Then this chat moves to past chats and a fresh one starts. Its assignments and background work carry on.',
      ),
    ).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveClass('kv-btn--secondary');
    await user.click(within(dialog).getByRole('button', { name: 'Start new chat' }));
    expect(await screen.findByText('Engineering lead is in the middle of a turn, so a new chat cannot start yet.')).toBeInTheDocument();
    expect(h.calls.some((c) => c.method === 'POST' && c.url === BASE + '/new-chat')).toBe(true);
  });

  it('starting a new chat follows the rotation and refetches when it is done', async () => {
    const user = userEvent.setup();
    const h = renderAgent({ chat: idle, routes: { ['POST ' + BASE + '/new-chat']: { started: true } } });
    await loaded();
    await user.click(screen.getByRole('button', { name: 'New chat' }));
    await user.click(await screen.findByRole('button', { name: 'Start new chat' }));
    await waitFor(() => expect(h.streams).toHaveLength(1));
    const before = gets(h, CHAT);
    h.emit('done', { waiting_tasks: 0, rotation: true });
    expect(gets(h, CHAT)).toBe(before);
    h.emit('rotation_done', {});
    await waitFor(() => expect(gets(h, CHAT)).toBe(before + 1));
  });
});

// A rotation never reloads the page: it clears and fetches, so each step of a rotation is
// something the screen has to show by itself: that a new chat was asked for, and then the fresh chat alone.
describe('Chat: a new chat (rotation)', () => {
  const talked: Chat = { ...idle, rows: goChat.rows.filter((r) => r.kind === 'message').slice(0, 2) };
  const fresh = freshChatAfter(talked);
  const NEW_CHAT = 'POST ' + BASE + '/new-chat';
  const BANNER = 'Engineering lead is starting a new chat.';
  const steps = sequences.rotation.steps;
  const DONE = steps.findIndex((s) => 'event' in s && s.event === 'done');

  /** The banner in the chat area, by its title (the page has other status regions: the dots, the agent's state). */
  function banner(): HTMLElement | null {
    return screen.queryByText(BANNER)?.closest<HTMLElement>('.kv-banner') ?? null;
  }

  async function startNewChat(user: ReturnType<typeof userEvent.setup>) {
    await user.click(screen.getByRole('button', { name: 'New chat' }));
    await user.click(await screen.findByRole('button', { name: 'Start new chat' }));
  }

  function expectFreshChat() {
    for (const gone of ['Where are we on the quote?', 'Checking now.', 'My memory is in the shape I want.', 'New chat requested', 'agent_memory_view', BANNER]) {
      expect(screen.queryByText(gone), gone).toBeNull();
    }
    expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull();
    expect(screen.queryByText(/^Thinking/)).toBeNull();
  }

  it('says so in the chat at once, follows the turn that folds it, then shows the fresh chat and nothing else', async () => {
    const user = userEvent.setup();
    let chat = talked;
    let detail = goAgentDetail;
    const h = renderAgent({ routes: { ['GET ' + CHAT]: () => chat, ['GET ' + BASE]: () => detail, [NEW_CHAT]: { started: true } } });
    await loaded();
    expect(transcript().getByText('Checking now.')).toBeInTheDocument();
    expect(screen.queryByText(BANNER)).toBeNull();

    await startNewChat(user);
    // At once, before the stream has said a word.
    await screen.findByText(BANNER);
    expect(banner()).toHaveAttribute('role', 'status');
    expect(banner()).toHaveTextContent('It is folding this chat into what it remembers. Messages you send now wait for the new chat.');
    // In the chat area, with the composer it speaks for.
    expect(banner()?.closest('.app-chat-compose')).toContainElement(composer());
    expect(screen.getByRole('button', { name: 'Stop' })).toBeInTheDocument();
    expect(transcript().getByText(/^Thinking/)).toBeInTheDocument();
    await waitFor(() => expect(h.streams).toHaveLength(1));

    // The request is a line in the transcript, below what was said, and the turn streams under it.
    h.play(steps.slice(0, DONE));
    expect(transcript().getAllByText('New chat requested')).toHaveLength(1);
    const items = transcript().getAllByRole('listitem').map((li) => li.textContent ?? '');
    expect(items.findIndex((t) => t.includes('Checking now.'))).toBeLessThan(items.findIndex((t) => t.includes('New chat requested')));
    expect(transcript().getByText('agent_memory_view')).toBeInTheDocument();
    expect(transcript().getByText('My memory is in the shape I want.')).toBeInTheDocument();

    // The turn ends; the chat is being archived. Nothing is fetched and the stream stays for rotation_done.
    const before = gets(h, CHAT);
    h.play(steps.slice(DONE, DONE + 1));
    expect(screen.getByText(BANNER)).toBeInTheDocument();
    expect(transcript().getByText('Checking now.')).toBeInTheDocument();
    expect(gets(h, CHAT)).toBe(before);
    expect(h.stream().stopped).toBe(false);

    // Archived. The server serves the fresh chat from here, and there is one more past chat.
    chat = fresh;
    detail = { ...goAgentDetail, counts: { ...goAgentDetail.counts, past_chats: goAgentDetail.counts.past_chats + 1 } };
    h.play(steps.slice(DONE + 1));
    expect(await screen.findByText('No messages yet')).toBeInTheDocument();
    expectFreshChat();
    expect(h.stream().stopped).toBe(true);
    expect(gets(h, CHAT)).toBe(before + 1);
    expect(composer()).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('tab', { name: /Past chats/ })).toHaveTextContent(String(goAgentDetail.counts.past_chats + 1)));
  });

  it('clears the previous chat the moment the rotation is done, before the fresh one has answered', async () => {
    const user = userEvent.setup();
    let answer: (r: Response) => void = () => {};
    let held = false;
    const h = renderAgent({
      routes: {
        ['GET ' + CHAT]: () => (held ? new Promise<Response>((resolve) => (answer = resolve)) : reply(200, talked)),
        [NEW_CHAT]: { started: true },
      },
    });
    await loaded();
    await startNewChat(user);
    await waitFor(() => expect(h.streams).toHaveLength(1));
    h.play(steps.slice(0, DONE + 1));
    held = true;
    h.play(steps.slice(DONE + 1));
    // The fetch is still out: the page is already empty, not showing the chat that was archived.
    expectFreshChat();
    expect(screen.queryByRole('list', { name: 'Chat with Engineering lead' })).toBeNull();
    expect(screen.queryByText('No messages yet')).toBeNull();
    await act(async () => {
      answer(reply(200, fresh));
      await Promise.resolve();
    });
    expect(await screen.findByText('No messages yet')).toBeInTheDocument();
  });

  it('a page that did not ask is told by the stream, once, and is cleared the same way', async () => {
    let chat = talked;
    const h = renderAgent({ routes: { ['GET ' + CHAT]: () => chat } });
    await loaded();
    h.snapshot(snapshotWith('running'));
    await waitFor(() => expect(h.streams).toHaveLength(1));
    // The marker, and its replay.
    h.play(steps.slice(0, 3));
    expect(transcript().getAllByText('New chat requested')).toHaveLength(1);
    expect(banner()).toBeInTheDocument();
    const following = h.stream();
    chat = fresh;
    h.play(steps.slice(3));
    expect(await screen.findByText('No messages yet')).toBeInTheDocument();
    expectFreshChat();
    expect(following.stopped).toBe(true);
  });

  it('a page opened mid-rotation says so from what the server answers', async () => {
    const stored: Chat = { ...talked, running: true, rotating: true, rows: [...talked.rows, { kind: 'rotation_prompt', ts: NOW - 1000, body_md: 'Reconcile your memory.' }] };
    const h = renderAgent({ chat: stored });
    await loaded();
    expect(banner()).toBeInTheDocument();
    expect(transcript().getAllByText('New chat requested')).toHaveLength(1);
    // The prompt is for the model: the page shows that it was asked, not the text.
    expect(screen.queryByText('Reconcile your memory.')).toBeNull();
    await waitFor(() => expect(h.streams).toHaveLength(1));
    // The stream replays the marker for the row the page already has.
    h.emit('chat_marker', { kind: 'rotation', content: '', ts: NOW - 1000 });
    expect(transcript().getAllByText('New chat requested')).toHaveLength(1);
  });

  it('a chat that rotated out of sight is replaced on the next fetch, what was painted live included', async () => {
    let chat: Chat = { ...talked, running: true };
    const h = renderAgent({ routes: { ['GET ' + CHAT]: () => chat } });
    await loaded();
    act(() => h.stream().setState('open'));
    h.play(steps.slice(1, DONE));
    expect(transcript().getByText('My memory is in the shape I want.')).toBeInTheDocument();
    // The connection dropped, and the rotation finished while it was down: rotation_done never came.
    chat = fresh;
    act(() => h.stream().setState('closed'));
    expect(await screen.findByText('No messages yet')).toBeInTheDocument();
    expectFreshChat();
  });

  it('the turn was over before the stream opened (a 204): the fetch settles it', async () => {
    const user = userEvent.setup();
    let chat = talked;
    const h = renderAgent({ routes: { ['GET ' + CHAT]: () => chat, [NEW_CHAT]: { started: true } } });
    await loaded();
    await startNewChat(user);
    await waitFor(() => expect(h.streams).toHaveLength(1));
    expect(await screen.findByText(BANNER)).toBeInTheDocument();
    chat = fresh;
    act(() => h.stream().setState('closed'));
    expect(await screen.findByText('No messages yet')).toBeInTheDocument();
    expectFreshChat();
  });

  it('a failed turn still ends in the fresh chat, and says what failed', async () => {
    const user = userEvent.setup();
    let chat = talked;
    const h = renderAgent({ routes: { ['GET ' + CHAT]: () => chat, [NEW_CHAT]: { started: true } } });
    await loaded();
    await startNewChat(user);
    await waitFor(() => expect(h.streams).toHaveLength(1));
    h.play(steps.slice(0, 3));
    const before = gets(h, CHAT);
    h.emit('error', { error: 'The model could not be reached.', rotation: true });
    expect(screen.getByText('The model could not be reached.')).toBeInTheDocument();
    // The chat is archived all the same: the stream stays for rotation_done and nothing is fetched yet.
    expect(h.stream().stopped).toBe(false);
    expect(gets(h, CHAT)).toBe(before);
    chat = fresh;
    h.emit('rotation_done', {});
    expect(await screen.findByText('No messages yet')).toBeInTheDocument();
    expectFreshChat();
    expect(screen.getByText('The model could not be reached.')).toBeInTheDocument();
  });

  it('starts nothing on an empty chat: no banner, no stream', async () => {
    const user = userEvent.setup();
    const h = renderAgent({ chat: idle, routes: { [NEW_CHAT]: { started: false } } });
    await loaded();
    await startNewChat(user);
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(h.calls.some((c) => c.method === 'POST' && c.url === BASE + '/new-chat')).toBe(true);
    expect(screen.queryByText(BANNER)).toBeNull();
    expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull();
    expect(h.streams).toHaveLength(0);
  });

  it('coming back to the chat tab after a rotation does not announce it again', async () => {
    const user = userEvent.setup();
    let chat = talked;
    const h = renderAgent({ routes: { ['GET ' + CHAT]: () => chat, [NEW_CHAT]: { started: true } } });
    await loaded();
    await startNewChat(user);
    await waitFor(() => expect(h.streams).toHaveLength(1));
    h.play(steps.slice(0, DONE + 1));
    chat = fresh;
    h.play(steps.slice(DONE + 1));
    expect(await screen.findByText('No messages yet')).toBeInTheDocument();
    const opened = h.streams.length;
    await user.click(screen.getByRole('tab', { name: /About/ }));
    await waitFor(() => expect(screen.getByRole('tab', { name: /About/ })).toHaveAttribute('aria-selected', 'true'));
    await user.click(screen.getByRole('tab', { name: /Chat/ }));
    expect(await screen.findByText('No messages yet')).toBeInTheDocument();
    expect(screen.queryByText(BANNER)).toBeNull();
    expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull();
    expect(h.streams).toHaveLength(opened);
  });
});

describe('Chat: the composer', () => {
  it('Enter sends, Shift+Enter adds a line, and the message paints only from chat_message (chat-stop-button, chat-message-dedup)', async () => {
    const user = userEvent.setup();
    const h = renderAgent({ chat: idle, routes: { ['POST ' + BASE + '/messages']: { ts: 1789923650000 } } });
    await loaded();
    await user.type(composer(), 'First line{Shift>}{Enter}{/Shift}second');
    expect(composer()).toHaveValue('First line\nsecond');
    expect(h.calls.some((c) => c.method === 'POST')).toBe(false);
    await user.type(composer(), '{Enter}');
    await waitFor(() => expect(h.calls.some((c) => c.method === 'POST' && c.url === BASE + '/messages')).toBe(true));
    const post = h.calls.find((c) => c.url === BASE + '/messages');
    expect(post?.form?.get('text')).toBe('First line\nsecond');
    await waitFor(() => expect(composer()).toHaveValue(''));
    // No optimistic bubble: the transcript is still empty.
    expect(screen.queryByText(/First line/)).toBeNull();
    // Stop shows at once, and the stream is open for the reply.
    expect(screen.getByRole('button', { name: 'Stop' })).toBeInTheDocument();
    h.emit('chat_message', { role: 'received', kind: 'direct_chat', content: 'First line\nsecond', ts: 1789923650000, attachments: [] });
    h.emit('chat_message', { role: 'received', kind: 'direct_chat', content: 'First line\nsecond', ts: 1789923650000, attachments: [] });
    expect(transcript().getAllByText(/First line/)).toHaveLength(1);
  });

  it('shows Stop only while running, POSTs stop and sends no message (chat-stop-button, chat-waiting-tasks)', async () => {
    const user = userEvent.setup();
    const h = renderAgent({ chat: { ...idle, running: true }, routes: { ['POST ' + BASE + '/stop']: { stopped: true } } });
    await loaded();
    const stop = await screen.findByRole('button', { name: 'Stop' });
    // Send stays available while the agent works.
    expect(composer()).not.toBeDisabled();
    await user.click(stop);
    await waitFor(() => expect(h.calls.some((c) => c.method === 'POST' && c.url === BASE + '/stop')).toBe(true));
    expect(h.calls.some((c) => c.url === BASE + '/messages')).toBe(false);
    h.emit('done', { waiting_tasks: 2 });
    // Tasks still run: Stop stays (it cancels them too).
    expect(screen.getByRole('button', { name: 'Stop' })).toBeInTheDocument();
    expect(screen.getByText(/^Waiting on 2 tasks/)).toBeInTheDocument();
    h.snapshot(snapshotWith('idle'));
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull());
    expect(screen.queryByText(/^Waiting on/)).toBeNull();
  });

  it('is absent while idle', async () => {
    renderAgent({ chat: idle });
    await loaded();
    expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull();
  });

  it('changes model and effort from the footer and shows what the server answered (agent-model-picker)', async () => {
    const user = userEvent.setup();
    const h = renderAgent({
      chat: goChat,
      routes: {
        ['POST ' + BASE + '/model']: { ...goChatSettings, current_model: 'claude-opus-4-1' },
        ['POST ' + BASE + '/effort']: { current_model: 'claude-opus-4-1', current_effort: 'low' },
      },
    });
    await loaded();
    const model = screen.getByRole('combobox', { name: 'Model' });
    expect(model).toHaveValue('claude-opus-5-5');
    expect(within(model).getByRole('option', { name: 'Opus 4.1 · pinned' })).toBeInTheDocument();
    await user.selectOptions(model, 'claude-opus-4-1');
    await waitFor(() => expect(h.calls.find((c) => c.url === BASE + '/model')?.json).toEqual({ model: 'claude-opus-4-1' }));
    await waitFor(() => expect(model).toHaveValue('claude-opus-4-1'));
    const effort = screen.getByRole('combobox', { name: 'Effort' });
    await user.selectOptions(effort, 'low');
    await waitFor(() => expect(effort).toHaveValue('low'));
    expect(h.calls.find((c) => c.url === BASE + '/effort')?.json).toEqual({ effort: 'low' });
  });

  // Models whose efforts differ, so each test can tell whose row the chip drew from. Ids are opaque.
  const twoModels: Chat = {
    ...idle,
    current_model: 'model-a',
    current_effort: '',
    models: [
      {
        id: 'model-a',
        label: 'Model A',
        legacy: false,
        current: true,
        context_window: 1000,
        provider: 'p',
        efforts: [
          { id: 'quick', label: 'Quick', default: false },
          { id: 'deep', label: 'Deep', default: true },
        ],
      },
      { id: 'model-b', label: 'Model B', legacy: false, current: true, context_window: 1000, provider: 'p', efforts: [] },
      {
        id: 'model-c',
        label: 'Model C',
        legacy: false,
        current: true,
        context_window: 1000,
        provider: 'p',
        efforts: [
          { id: 'one', label: 'One', default: true },
          { id: 'two', label: 'Two', default: false },
          { id: 'three', label: 'Three', default: false },
        ],
      },
    ],
  };
  const optionLabels = (select: HTMLElement) => within(select).getAllByRole('option').map((o) => [o.getAttribute('value'), o.textContent]);

  it("draws the effort chip from the current model's efforts, selecting the default when none is set", async () => {
    renderAgent({ chat: twoModels });
    await loaded();
    const effort = screen.getByRole('combobox', { name: 'Effort' });
    expect(optionLabels(effort)).toEqual([
      ['quick', 'Quick'],
      ['deep', 'Deep'],
    ]);
    expect(effort).toHaveValue('deep');
  });

  it('hides the effort chip when the current model offers no efforts', async () => {
    renderAgent({ chat: { ...twoModels, current_model: 'model-b' } });
    await loaded();
    expect(screen.getByRole('combobox', { name: 'Model' })).toHaveValue('model-b');
    expect(screen.queryByRole('combobox', { name: 'Effort' })).toBeNull();
  });

  it('hides the effort chip when the current model is not among the models', async () => {
    renderAgent({ chat: { ...twoModels, current_model: 'model-gone', current_effort: 'deep' } });
    await loaded();
    expect(screen.queryByRole('combobox', { name: 'Effort' })).toBeNull();
  });

  it("re-fills the effort chip from the newly picked model's row", async () => {
    const user = userEvent.setup();
    const h = renderAgent({
      chat: { ...twoModels, current_effort: 'quick' },
      routes: { ['POST ' + BASE + '/model']: { current_model: 'model-c', current_effort: 'two' } },
    });
    await loaded();
    expect(screen.getByRole('combobox', { name: 'Effort' })).toHaveValue('quick');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Model' }), 'model-c');
    await waitFor(() => expect(screen.getByRole('combobox', { name: 'Model' })).toHaveValue('model-c'));
    expect(h.calls.find((c) => c.url === BASE + '/model')?.json).toEqual({ model: 'model-c' });
    const effort = screen.getByRole('combobox', { name: 'Effort' });
    expect(optionLabels(effort)).toEqual([
      ['one', 'One'],
      ['two', 'Two'],
      ['three', 'Three'],
    ]);
    expect(effort).toHaveValue('two');
  });

  it('stages attachments across picks and sends them multipart (chat-attachments)', async () => {
    const user = userEvent.setup();
    const h = renderAgent({ chat: idle, routes: { ['POST ' + BASE + '/messages']: { ts: 5, attachments: [{ sha: 'a', name: 'hosting-quote.pdf' }] } } });
    await loaded();
    await user.click(screen.getByRole('button', { name: 'Attach files' }));
    await screen.findByText('notes.md');
    // Picking the same files again does not stage them twice.
    await user.click(screen.getByRole('button', { name: 'Attach files' }));
    expect(screen.getAllByText('notes.md')).toHaveLength(1);
    await user.click(screen.getByRole('button', { name: 'Remove notes.md' }));
    expect(screen.queryByText('notes.md')).toBeNull();
    await user.type(composer(), 'The quote{Enter}');
    await waitFor(() => expect(h.calls.some((c) => c.url === BASE + '/messages')).toBe(true));
    const form = h.calls.find((c) => c.url === BASE + '/messages')?.form;
    expect(form?.get('text')).toBe('The quote');
    expect(form?.getAll('attachment').map((f) => (f as File).name)).toEqual(['hosting-quote.pdf']);
    await waitFor(() => expect(screen.queryByText('hosting-quote.pdf')).toBeNull());
  });

  it('sends staged files with no text (chat-attachments: only attachments still posts)', async () => {
    const user = userEvent.setup();
    const h = renderAgent({ chat: idle, routes: { ['POST ' + BASE + '/messages']: { ts: 6, attachments: [{ sha: 'a', name: 'hosting-quote.pdf' }] } } });
    await loaded();
    // Nothing staged, nothing typed: Send stays off.
    expect(screen.getByRole('button', { name: 'Send' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: 'Attach files' }));
    await screen.findByText('notes.md');
    expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(h.calls.some((c) => c.url === BASE + '/messages')).toBe(true));
    const form = h.calls.find((c) => c.url === BASE + '/messages')?.form;
    expect(form?.get('text')).toBe('');
    expect(form?.getAll('attachment').map((f) => (f as File).name)).toEqual(['hosting-quote.pdf', 'notes.md']);
    await waitFor(() => expect(screen.queryByText('notes.md')).toBeNull());
  });

  it('keeps the text and staged files when the send fails', async () => {
    const user = userEvent.setup();
    renderAgent({ chat: idle, routes: { ['POST ' + BASE + '/messages']: reply(500, { error: 'The message could not be saved.', who: 'Whoever runs this Kivali server.' }) } });
    await loaded();
    await user.type(composer(), 'Hello{Enter}');
    expect(await screen.findByText('The message could not be saved.')).toBeInTheDocument();
    expect(composer()).toHaveValue('Hello');
  });

  it('keeps a draft per agent across a reload', async () => {
    const user = userEvent.setup();
    renderAgent({ chat: idle });
    await loaded();
    await user.type(composer(), 'Half a thought');
    const saved = JSON.parse(window.localStorage.getItem('kivali.draft.engineering-lead') ?? '{}') as { v?: string };
    expect(saved.v).toBe('Half a thought');
  });

  it('restores a saved draft', async () => {
    window.localStorage.setItem('kivali.draft.engineering-lead', JSON.stringify({ v: 'Saved words', ts: NOW }));
    renderAgent({ chat: idle });
    await loaded();
    expect(composer()).toHaveValue('Saved words');
  });

  it('is gone in read-only mode, with the pending messages', async () => {
    renderAgent({ chat: { ...goChat, archived: true, running: false }, detail: { ...goAgentDetail, archived: true } });
    await loaded();
    expect(screen.queryByRole('textbox', { name: 'Message Engineering lead' })).toBeNull();
    expect(screen.queryByText('Also check the staging deploy.')).toBeNull();
    expect(screen.queryByRole('button', { name: 'New chat' })).toBeNull();
    expect(screen.getByText('Archived')).toBeInTheDocument();
    // Everything else is the same.
    expect(transcript().getByText('Checking now.')).toBeInTheDocument();
  });
});

describe('Chat: pending messages', () => {
  const pendingChat: Chat = { ...idle, running: true, pending: [goPendingMessage] };

  it('Delete → toast with Undo → restore brings it back', async () => {
    const user = userEvent.setup();
    const h = renderAgent({
      chat: pendingChat,
      routes: {
        ['DELETE ' + BASE + '/pending/' + goPendingMessage.id]: reply(204),
        ['POST ' + BASE + '/pending/' + goPendingMessage.id + '/restore']: { ...goPendingRestore, delivered_ts: undefined },
      },
    });
    await loaded();
    await user.click(transcript().getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(screen.queryByText('Also check the staging deploy.')).toBeNull());
    expect(screen.getByText('Message deleted')).toBeInTheDocument();
    expect(screen.getByText('It was never delivered to Engineering lead.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Undo' }));
    expect(await screen.findByText('Also check the staging deploy.')).toBeInTheDocument();
    expect(h.calls.some((c) => c.method === 'POST' && c.url.endsWith('/restore'))).toBe(true);
    expect(screen.queryByText('Message deleted')).toBeNull();
  });

  it('says so when it is too late to restore (410)', async () => {
    const user = userEvent.setup();
    renderAgent({
      chat: pendingChat,
      routes: {
        ['DELETE ' + BASE + '/pending/' + goPendingMessage.id]: reply(204),
        ['POST ' + BASE + '/pending/' + goPendingMessage.id + '/restore']: reply(410, { error: 'grace_expired', who: 'no one' }),
      },
    });
    await loaded();
    await user.click(transcript().getByRole('button', { name: 'Delete' }));
    await user.click(await screen.findByRole('button', { name: 'Undo' }));
    expect(await screen.findByText('Too late to restore')).toBeInTheDocument();
  });

  it('hides Delete once the turn has been handed the message, and after a 409', async () => {
    const user = userEvent.setup();
    const h = renderAgent({ chat: { ...pendingChat, pending: [goPendingMessage, { ...goPendingMessage, id: 'second', text: 'Second one.' }] }, routes: { ['DELETE ' + BASE + '/pending/second']: reply(409, { error: 'already_with_agent', who: 'x' }) } });
    await loaded();
    expect(transcript().getAllByRole('button', { name: 'Delete' })).toHaveLength(2);
    h.emit('pending_offered', { id: goPendingMessage.id });
    expect(transcript().getAllByRole('button', { name: 'Delete' })).toHaveLength(1);
    await user.click(transcript().getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(transcript().queryByRole('button', { name: 'Delete' })).toBeNull());
    expect(screen.getByText('Already with Engineering lead')).toBeInTheDocument();
    expect(transcript().getByText('Second one.')).toBeInTheDocument();
  });

  it('Send now shows its loading state until the delivery, which lands after the pause marker', async () => {
    const user = userEvent.setup();
    const h = renderAgent({ chat: pendingChat, routes: { ['POST ' + BASE + '/pending/' + goPendingMessage.id + '/send-now']: reply(202, {}) } });
    await loaded();
    h.emit('delta', { text: 'Asking Provider A', start_ts: NOW - 5000, model: 'Opus 5.5', effort: 'high' });
    const sendNow = transcript().getByRole('button', { name: 'Send now' });
    await user.click(sendNow);
    await waitFor(() => expect(sendNow).toHaveAttribute('aria-busy', 'true'));
    h.emit('chat_marker', { kind: 'paused-to-deliver', content: 'Paused to deliver your message', ts: NOW });
    h.emit('pending_delivered', { id: goPendingMessage.id, ts: goPendingMessage.queued_at });
    h.emit('chat_message', { role: 'received', kind: 'direct_chat', content: goPendingMessage.text, ts: goPendingMessage.queued_at, attachments: [] });
    expect(screen.queryByText('Pending')).toBeNull();
    const items = transcript().getAllByRole('listitem').map((li) => li.textContent ?? '');
    const reply0 = items.findIndex((t) => t.includes('Asking Provider A'));
    const marker = items.findIndex((t) => t.includes('Paused to deliver your message'));
    const delivered = items.findIndex((t) => t.includes(goPendingMessage.text));
    expect(reply0).toBeLessThan(marker);
    expect(marker).toBeLessThan(delivered);
    expect(transcript().getAllByText(goPendingMessage.text)).toHaveLength(1);
  });

  it('paints a pending row from the POST when the agent is mid-turn', async () => {
    const user = userEvent.setup();
    renderAgent({ chat: { ...idle, running: true }, routes: { ['POST ' + BASE + '/messages']: { ts: NOW, pending_id: 'p9' } } });
    await loaded();
    await user.type(composer(), 'While you work{Enter}');
    expect(await screen.findByText('While you work')).toBeInTheDocument();
    expect(screen.getByText('Pending')).toBeInTheDocument();
  });
});

describe('Chat: the live stream', () => {
  it('a recorded turn drives the screen: message, dots, reply, tool call, done (FakeStream + transcript-full-turn.json)', async () => {
    let calls = 0;
    const h = renderAgent({ routes: { ['GET ' + CHAT]: () => (++calls === 1 ? { ...idle, running: true } : idle) } });
    await loaded();
    h.play(sequences.fullTurn.steps.slice(0, 6));
    expect(transcript().getByText('Where are we on the quote?')).toBeInTheDocument();
    expect(transcript().getByText(/^Thinking/)).toBeInTheDocument();
    h.play(sequences.fullTurn.steps.slice(9, 10));
    // Text is arriving: the caret shows and the dots do not.
    expect(transcript().queryByText(/^Thinking/)).toBeNull();
    expect(transcript().getByText('cheapest').tagName).toBe('STRONG');
    expect(document.querySelector('.kv-caret')).not.toBeNull();
    const before = gets(h, CHAT);
    h.play(sequences.fullTurn.steps.slice(10));
    expect(transcript().getByText('assignment_view')).toBeInTheDocument();
    expect(transcript().getByText('Read #42')).toBeInTheDocument();
    expect(document.querySelector('.kv-caret')).toBeNull();
    // done ends the turn and refetches the chat to settle on the server's copy.
    await waitFor(() => expect(gets(h, CHAT)).toBe(before + 1));
    expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull();
  });

  it('replayed deltas after a reopen rebuild one reply (chat-delta-replay via transcript-delta-replay.json)', async () => {
    const h = renderAgent({ chat: { ...idle, running: true } });
    await loaded();
    h.play(sequences.deltaReplay.steps);
    expect(transcript().getByText('First, the release notes. Done.')).toBeInTheDocument();
    expect(transcript().getByText('Next, the requirements.')).toBeInTheDocument();
  });

  it('background tasks update in place and name their model (subagent-task-pills via transcript-subagents.json)', async () => {
    let calls = 0;
    const h = renderAgent({ chat: undefined, routes: { ['GET ' + CHAT]: () => (++calls === 1 ? { ...idle, running: true } : idle) } });
    await loaded();
    h.play(sequences.subagents.steps.slice(0, 4));
    expect(transcript().getByText('2 subagents')).toBeInTheDocument();
    expect(transcript().getByText('sonnet 4.6 · low')).toBeInTheDocument();
    expect(transcript().getByText('running WebFetch')).toBeInTheDocument();
    h.play(sequences.subagents.steps.slice(4, 6));
    expect(transcript().getByText('running file_view')).toBeInTheDocument();
    // done with tasks outstanding: the dots say so and the stream closes; the snapshot carries on from here.
    h.play(sequences.subagents.steps.slice(6, 7));
    expect(transcript().getByText(/^Waiting on 2 tasks/)).toBeInTheDocument();
    expect(h.stream().stopped).toBe(true);
    const before = gets(h, CHAT);
    h.snapshot(snapshotWith('waiting', 40, goSnapshot, 2));
    expect(transcript().getByText(/^Waiting on 2 tasks/)).toBeInTheDocument();
    // The count follows the tasks down.
    h.snapshot(snapshotWith('waiting', 40, goSnapshot, 1));
    expect(transcript().getByText(/^Waiting on 1 task/)).toBeInTheDocument();
    // The batch finished: the dots go and the rows settle from the server.
    h.snapshot(snapshotWith('idle'));
    await waitFor(() => expect(gets(h, CHAT)).toBe(before + 1));
    expect(transcript().queryByText(/^Waiting on/)).toBeNull();
  });

  it('a turn error is a marker; the error event shows a banner and paints nothing else (chat-turn-error-marker)', async () => {
    const h = renderAgent({ chat: { ...idle, running: true } });
    await loaded();
    h.play(sequences.turnError.steps);
    expect(transcript().getByText('Stopped on an error')).toBeInTheDocument();
    expect(screen.getByText("The model's usage limit was reached.")).toBeInTheDocument();
    expect(h.stream().stopped).toBe(true);
  });

  it('reopens the stream when the refetch after done still says running, and the 204 takes Stop away', async () => {
    const h = renderAgent({ chat: { ...idle, running: true } });
    await loaded();
    expect(h.streams).toHaveLength(1);
    // The server's in-flight mark outlives its done by a moment: the refetch still says running.
    h.emit('done', { waiting_tasks: 0 });
    await waitFor(() => expect(gets(h, CHAT)).toBe(2));
    await waitFor(() => expect(h.streams).toHaveLength(2));
    expect(h.stream().stopped).toBe(false);
    act(() => h.stream().setState('closed'));
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull());
  });

  it('swallows the 204 a stream gets when nothing is running', async () => {
    const h = renderAgent({ chat: { ...idle, running: true } });
    await loaded();
    act(() => h.stream().setState('closed'));
    h.emit('error', '');
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull();
  });
});

describe('Chat: following the org snapshot (chat-thinking-reconcile)', () => {
  it('opens a stream when the snapshot says the agent is working and none is open, and does not churn one that is', async () => {
    const h = renderAgent({ chat: idle });
    await loaded();
    expect(h.streams).toHaveLength(0);
    h.snapshot(snapshotWith('running'));
    await waitFor(() => expect(h.streams).toHaveLength(1));
    h.snapshot(snapshotWith('running'));
    expect(h.streams).toHaveLength(1);
    // A different agent's state changing is not this chat's business.
    h.snapshot({ ...snapshotWith('running'), agents: snapshotWith('running').agents.map((a) => (a.slug === 'buyer' ? { ...a, state: 'idle' as const } : a)) });
    expect(h.streams).toHaveLength(1);
  });

  it('reopens after the stream dropped mid-session', async () => {
    const h = renderAgent({ chat: { ...idle, running: true } });
    await loaded();
    act(() => h.stream().setState('closed'));
    h.snapshot(snapshotWith('running'));
    await waitFor(() => expect(h.streams).toHaveLength(2));
  });

  it('after the tab comes back: force-reopens for a working agent, refetches a turn that ended while away, and is one-shot', async () => {
    const h = renderAgent({ chat: { ...idle, running: true } });
    await loaded();
    const visibility = (v: DocumentVisibilityState) => {
      Object.defineProperty(document, 'visibilityState', { configurable: true, value: v });
      act(() => document.dispatchEvent(new Event('visibilitychange')));
    };
    visibility('hidden');
    visibility('visible');
    h.snapshot(snapshotWith('running'));
    await waitFor(() => expect(h.streams).toHaveLength(2));
    expect(h.streams[0]?.stopped).toBe(true);
    // One-shot: the next snapshot in the same window does nothing.
    h.snapshot(snapshotWith('running'));
    expect(h.streams).toHaveLength(2);
    // Mid-turn when hidden, finished when back: refetch the chat.
    visibility('hidden');
    visibility('visible');
    const before = gets(h, CHAT);
    h.snapshot(snapshotWith('idle'));
    await waitFor(() => expect(gets(h, CHAT)).toBe(before + 1));
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
  });

  it('an agent held after Stop is "waiting" with no tasks: no dots, no Stop, and the header says so', async () => {
    renderAgent({ chat: idle, snapshot: snapshotWith('waiting', 40, goSnapshot, 0) });
    await loaded();
    expect(screen.queryByText(/^Waiting on/)).toBeNull();
    expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull();
    expect(screen.getAllByText('Stopped by you').length).toBeGreaterThan(0);
  });

  it('refetches when a connected stream ends mid-turn without done', async () => {
    const h = renderAgent({ chat: { ...idle, running: true } });
    await loaded();
    act(() => h.stream().setState('open'));
    h.emit('delta', { text: 'Half a', start_ts: NOW - 1000, model: 'Opus 5.5', effort: 'high' });
    const before = gets(h, CHAT);
    // The browser reconnected to a hub that was gone (204): the turn ended while the connection was down.
    act(() => h.stream().setState('closed'));
    await waitFor(() => expect(gets(h, CHAT)).toBe(before + 1));
  });

  it('a snapshot without this agent is no information: no reopen, no refetch', async () => {
    const h = renderAgent({ chat: idle });
    await loaded();
    const before = gets(h, CHAT);
    h.snapshot({ ...snapshotWith('idle'), agents: snapshotWith('idle').agents.filter((a) => a.slug !== SLUG) });
    expect(h.streams).toHaveLength(0);
    expect(gets(h, CHAT)).toBe(before);
  });
});
