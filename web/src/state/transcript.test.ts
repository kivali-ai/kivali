import { describe, expect, it } from 'vitest';
import type { Chat, SubagentTranscript } from '../api/types.gen';
import {
  emptyChat,
  freshChatAfter,
  goChat,
  goChatMessage,
  goChatMessageTaskResult,
  goMessagePost,
  goPendingDelivered,
  goPendingMessage,
  goPendingRestore,
  goSubagentTranscript,
  sequences,
  stepAction,
} from './fixtures/transcript';
import type { Sequence } from './fixtures/transcript';
import {
  canStop,
  currentEfforts,
  initialTranscript,
  isSentSide,
  isStreaming,
  mergeLoaded,
  reconcileSnapshot,
  reduceTranscript,
  selectedEffort,
  thinkingLine,
  thinkingView,
  toBlocks,
} from './transcript';
import type { ChatRow, TranscriptAction, TranscriptState } from './transcript';

const AGENT = { slug: 'engineering-lead', name: 'Engineering lead' };
const AT = 1789923600000;

function start(chat: Chat = emptyChat): TranscriptState {
  return reduceTranscript(initialTranscript(AGENT), { type: 'loaded', chat, at: AT });
}

function run(state: TranscriptState, actions: TranscriptAction[]): TranscriptState {
  return actions.reduce(reduceTranscript, state);
}

function ev(name: string, data: unknown, at = AT): TranscriptAction {
  return { type: 'event', name, data, at };
}

/** Plays a recorded sequence, checking the invariants that must hold after every step. */
function play(seq: Sequence, state = start()): TranscriptState {
  let s = state;
  seq.steps.forEach((step, i) => {
    s = reduceTranscript(s, stepAction(step, AT + i));
    // Running dots and the caret never show together.
    expect(isStreaming(s) && thinkingView(s) !== null, `dots and caret together after step ${i}`).toBe(false);
    // At most one reply is open for deltas.
    expect(s.rows.filter((r) => r.open).length).toBeLessThanOrEqual(1);
    // Keys are unique.
    expect(new Set(s.rows.map((r) => r.key)).size).toBe(s.rows.length);
  });
  return s;
}

const replies = (s: TranscriptState) => s.rows.filter((r) => r.kind === 'message' && r.role === 'received');
const sent = (s: TranscriptState) => s.rows.filter((r) => r.kind === 'message' && r.role === 'sent');

describe('loaded', () => {
  it('takes the golden chat as it is: rows keyed, pending, fill, settings', () => {
    const s = start(goChat);
    expect(s.loaded).toBe(true);
    expect(s.rows.map((r) => r.kind)).toEqual(goChat.rows.map((r) => r.kind));
    expect(new Set(s.rows.map((r) => r.key)).size).toBe(s.rows.length);
    expect(s.pending).toHaveLength(1);
    expect(s.fill?.pct).toBe(82);
    expect(s.settings.currentModel).toBe('claude-opus-5-5');
    expect(currentEfforts(s.settings).map((e) => e.id)).toContain('max');
  });

  it("takes the effort options from the current model's row, and its default when the effort is not offered", () => {
    const models = [
      { id: 'm-a', label: 'A', legacy: false, current: true, context_window: 1000, provider: 'p', efforts: [{ id: 'lo', label: 'Low', default: false }, { id: 'hi', label: 'High', default: true }] },
      { id: 'm-b', label: 'B', legacy: false, current: true, context_window: 1000, provider: 'p', efforts: [] },
    ];
    expect(currentEfforts({ models, currentModel: 'm-a', currentEffort: 'lo' }).map((e) => e.label)).toEqual(['Low', 'High']);
    expect(selectedEffort({ models, currentModel: 'm-a', currentEffort: 'lo' })).toBe('lo');
    expect(selectedEffort({ models, currentModel: 'm-a', currentEffort: '' })).toBe('hi');
    expect(selectedEffort({ models, currentModel: 'm-a', currentEffort: 'unknown' })).toBe('hi');
    expect(currentEfforts({ models, currentModel: 'm-b', currentEffort: '' })).toEqual([]);
    expect(currentEfforts({ models, currentModel: 'm-gone', currentEffort: 'hi' })).toEqual([]);
    expect(selectedEffort({ models, currentModel: 'm-gone', currentEffort: 'hi' })).toBe('');
  });

  it('starts a running chat with tasks outstanding in waiting mode (chat-waiting-tasks: adopts the server-rendered count)', () => {
    const s = start(goChat);
    expect(s.waitingTasks).toBe(1);
    expect(thinkingView(s)?.label).toBe('Waiting on 1 task');
    expect(canStop(s)).toBe(true);
  });

  it('keeps a reply that is still streaming across a refetch, and drops what the server now holds', () => {
    let s = run(start(), [ev('delta', { text: 'Half a', start_ts: 5, model: 'Opus 5.5' })]);
    s = reduceTranscript(s, { type: 'loaded', chat: { ...goChat, running: true, waiting_tasks: 0 }, at: AT });
    expect(s.rows.at(-1)?.body_md).toBe('Half a');
    expect(s.rows).toHaveLength(goChat.rows.length + 1);
    // Once the turn is over, the server's copy is the only copy.
    s = run(s, [ev('done', { waiting_tasks: 0 })]);
    s = reduceTranscript(s, { type: 'loaded', chat: goChat, at: AT });
    expect(s.rows).toHaveLength(goChat.rows.length);
  });

  it('draws a task result as a row of its own and groups an agent turn under one message', () => {
    const blocks = toBlocks(start(goChat).rows, { kind: 'agent', slug: 'engineering-lead', name: 'Engineering lead' });
    const texts = blocks.map((b) => (b.type === 'row' ? b.row.kind : 'agent:' + b.items.map((i) => i.kind).join(',')));
    expect(texts).toEqual([
      'message',
      'agent:',
      'agent:',
      'message',
      'delivery',
      'ceo_queue',
      'agent:tool_use,tool_use,tool_use,tool_use,doc_published,file_shared,subagents',
      // One that names its task, one that does not: neither joins the agent's turn above.
      'task_result',
      'task_result',
      'marker',
      'marker',
      'rotation_prompt',
    ]);
    expect(toBlocks(start(goChat).rows, { kind: 'agent', slug: 'x', name: 'x' }, { showWake: true }).some((b) => b.type === 'row' && b.row.kind === 'wake_update')).toBe(true);
  });
});

describe('deltas (chat-delta-replay, chat-model-switch)', () => {
  it('two reconnects during one message leave one reply with the text once, and live deltas append to it', () => {
    const s = play(sequences.deltaReplay);
    const r = replies(s);
    expect(r).toHaveLength(2);
    expect(r[0]?.ts).toBe(1789923600000);
    expect(r[0]?.body_md).toBe('First, the release notes. Done.');
    // A new message after a reconnect still gets its own reply.
    expect(r[1]?.ts).toBe(1789923605000);
    expect(r[1]?.body_md).toBe('Next, the requirements.');
  });

  it('keeps one reply while the model stays the same and splits when it changes', () => {
    const s = play(sequences.modelSwitch);
    const r = replies(s);
    expect(r.map((x) => x.model)).toEqual(['Fable 5.1', 'Opus 5']);
    expect(r[0]?.body_md).toBe('from fable still fable');
    expect(r[1]?.body_md).toBe('from opus');
  });

  it('does not split when the server omits the model', () => {
    const s = run(start(), [ev('delta', { text: 'alpha ', start_ts: 1000 }), ev('delta', { text: 'beta', start_ts: 1000 })]);
    expect(replies(s)).toHaveLength(1);
    expect(replies(s)[0]?.body_md).toBe('alpha beta');
    expect(replies(s)[0]?.model).toBeUndefined();
  });

  it('shows the caret while text arrives and never the dots with it', () => {
    let s = run(start(), [ev('thinking', { step: 1 })]);
    expect(thinkingView(s)).not.toBeNull();
    s = run(s, [ev('delta', { text: 'Hi', start_ts: 1 })]);
    expect(isStreaming(s)).toBe(true);
    expect(thinkingView(s)).toBeNull();
    // Reasoning between two text blocks of the same buffered reply: dots back, caret gone, same reply.
    s = run(s, [ev('thinking', { step: 1 })]);
    expect(isStreaming(s)).toBe(false);
    expect(thinkingView(s)).not.toBeNull();
    s = run(s, [ev('delta', { text: ' there', start_ts: 1 })]);
    expect(replies(s)).toHaveLength(1);
    expect(replies(s)[0]?.body_md).toBe('Hi there');
  });
});

describe('thinking (chat-thinking-summary)', () => {
  it('counts steps only once the debounce promotes them, and never goes backwards on replay', () => {
    let s = run(start(), [ev('thinking', { step: 1 })]);
    expect(thinkingView(s)?.label).toBe('Thinking');
    s = run(s, [{ type: 'thinking_promote' }]);
    expect(thinkingView(s)?.label).toBe('Thinking · 1 step');
    s = run(s, [ev('thinking', { step: 3 }), ev('thinking', { step: 2 }), { type: 'thinking_promote' }]);
    expect(thinkingView(s)?.label).toBe('Thinking · 3 steps');
  });

  it('does not flash a step when a message lands right behind it', () => {
    const s = run(start(), [ev('thinking', { step: 1, summary: 'about to answer' }), ev('delta', { text: 'the answer', start_ts: 9 }), { type: 'thinking_promote' }, ev('tool_use_start', { tool_use_id: 't', tool_name: 'file_view', start_ts: 10 })]);
    expect(thinkingView(s)?.label).toBe('Thinking');
  });

  it('appends the gist, and resets per interval on a reply or a tool call', () => {
    let s = run(start(), [ev('thinking', { step: 2, summary: 'Checking the budget.' }), { type: 'thinking_promote' }]);
    expect(thinkingView(s)?.label).toBe('Thinking · 2 steps — Checking the budget.');
    s = run(s, [ev('tool_use_start', { tool_use_id: 'tu-1', tool_name: 'file_view', start_ts: AT + 5000 })]);
    expect(thinkingView(s)?.label).toBe('Thinking');
    expect(thinkingView(s)?.since).toBe(AT + 5000);
    s = run(s, [ev('thinking', { step: 1 }), { type: 'thinking_promote' }]);
    expect(thinkingView(s)?.label).toBe('Thinking · 1 step');
  });

  it('shows the elapsed clock anchored to the interval start', () => {
    const s = run(start(), [ev('delta', { text: 'x', start_ts: AT - 90_000 }), ev('tool_use_start', { tool_use_id: 'a', tool_name: 'x', start_ts: AT - 90_000 })]);
    const view = thinkingView(s);
    expect(view && thinkingLine(view, AT)).toBe('Thinking · 1:30');
  });
});

describe('tool calls (chat-history-group)', () => {
  it('update in place through start, input deltas, use and result, and ignore a replay after the result', () => {
    const s = play(sequences.toolLifecycle);
    const tools = s.rows.filter((r) => r.kind === 'tool_use');
    expect(tools).toHaveLength(2);
    const [read, shell] = tools as [ChatRow, ChatRow];
    expect(read.status).toBe('done');
    expect(read.input).toBe('{"path":"/files/quote.md"}');
    expect(read.output).toBe('Provider A: $92 a month');
    expect(read.summary).toBe('Read /files/quote.md');
    expect(shell.status).toBe('error');
    expect(shell.output).toBe('no such file');
    // The reply before the tools was closed by the first tool call.
    expect(replies(s)[0]?.open).toBe(false);
  });

  it('shows the input as it streams and summarizes from partial JSON', () => {
    const s = run(start(), [
      ev('tool_use_start', { tool_use_id: 'w', tool_name: 'file_create', start_ts: 1 }),
      ev('tool_input_delta', { tool_use_id: 'w', text: '{"path":"/files/role.md","file_text":"# Role\\n## Hab' }),
    ]);
    const row = s.rows[0];
    expect(row?.input).toBe('# Role\n## Hab');
    expect(row?.summary).toBe('Writing /files/role.md — Hab');
  });

  it('turns a publish call into the published document and a share into the shared files', () => {
    const s = run(start(), [
      ev('tool_use_start', { tool_use_id: 'p', tool_name: 'publish_notice', start_ts: 1 }),
      ev('doc_published', {
        tool_use_id: 'p',
        doc_type: 'notice',
        doc_title: 'Quote accepted',
        doc_to: 'buyer, test-runner',
        doc_path: 'messages/x.md',
        doc_body: 'We take the quote.',
        doc_files: [{ sha: 'q1', name: 'quote.pdf' }],
        doc_assignment: 42,
      }),
      ev('tool_use_start', { tool_use_id: 'f', tool_name: 'share_file', start_ts: 2 }),
      ev('file_shared', { tool_use_id: 'f', files: [{ sha: 's1', name: 'quote.pdf' }], caption: '' }),
    ]);
    expect(s.rows.map((r) => r.kind)).toEqual(['doc_published', 'file_shared']);
    // The stored file is the raw link inside the card; the row itself carries the message.
    // Recipients arrive as the one string the publish call wrote.
    expect(s.rows[0]?.to?.map((p) => p.slug)).toEqual(['buyer', 'test-runner']);
    expect(s.rows[0]?.raw_url).toBe('/messages/x.md');
    expect(s.rows[0]?.body_md).toBe('We take the quote.');
    expect(s.rows[0]?.attachments?.[0]?.url).toBe('/attachments/q1');
    expect(s.rows[0]?.assignment_ref).toEqual({ id: 42, title: '' });
    expect(s.rows[1]?.body_md).toBe('Shared file: quote.pdf');
    expect(s.rows[1]?.attachments?.[0]?.url).toBe('/attachments/s1');
  });
});

describe('background tasks (subagent-task-pills, chat-waiting-tasks)', () => {
  it('update through started, activity, progress and completed; a result message is painted where it landed', () => {
    const s = play(sequences.subagents);
    const batches = s.rows.filter((r) => r.kind === 'subagents');
    expect(batches).toHaveLength(1);
    const tasks = batches[0]?.tasks ?? [];
    expect(tasks.map((t) => [t.title, t.model, t.effort, t.state])).toEqual([
      ['Compare hosting providers', 'Sonnet 4.6', 'low', 'done'],
      ['Check the build logs', 'Haiku 4.5', 'medium', 'errored'],
    ]);
    expect(tasks[0]?.output_md).toBe('Three providers quote under $4k.');
    // Activity after the task finished does not reopen it.
    expect(tasks[0]?.activity).toBe('');
    expect(tasks[1]?.error).toBe('The agent pod went away');
    // The result is a row of its own, after the batch: the task as it ended. It is not a message from you.
    expect(s.rows.map((r) => r.kind)).toEqual(['subagents', 'task_result']);
    expect(s.rows[1]?.ts).toBe(1789923700000);
    expect(s.rows[1]?.tasks?.map((t) => [t.id, t.title, t.state, t.output_md])).toEqual([['aaaa1111', 'Compare hosting providers', 'done', 'Three providers quote under $4k.']]);
    expect(sent(s)).toHaveLength(0);
  });

  it('paints the result the server emits once, and the GET that brings the same row finds it there (chat_message_task_result.json)', () => {
    const row = goChatMessageTaskResult.row;
    if (!row) throw new Error('the golden event carries no row');
    // The hub replays the event when the stream reopens.
    let s = run(start(), [ev('chat_message', goChatMessageTaskResult), ev('chat_message', goChatMessageTaskResult)]);
    expect(s.rows.map((r) => r.kind)).toEqual(['task_result']);
    expect(s.rows[0]?.ts).toBe(row.ts);
    expect(s.rows[0]?.tasks).toEqual(row.tasks);
    // It woke the agent: a turn is running.
    expect(s.running).toBe(true);
    s = run(s, [ev('delta', { text: 'The survey is back.', start_ts: row.ts + 1000 }), ev('done', { waiting_tasks: 0 })]);
    s = reduceTranscript(s, {
      type: 'loaded',
      chat: { ...emptyChat, rows: [row, { kind: 'message', ts: row.ts + 2000, role: 'received', from: s.agent, body_md: 'The survey is back.', attachments: [], pending: false }] },
      at: AT,
    });
    expect(s.rows.map((r) => r.kind)).toEqual(['task_result', 'message']);
  });

  it('paints a result whose batch is not on the page, below the text the agent had written', () => {
    const s = run(start(), [ev('delta', { text: 'Working on it.', start_ts: 5 }), ev('chat_message', goChatMessageTaskResult), ev('delta', { text: 'The survey is back.', start_ts: 9 })]);
    expect(s.rows.map((r) => r.kind)).toEqual(['message', 'task_result', 'message']);
    expect(replies(s).map((r) => r.body_md)).toEqual(['Working on it.', 'The survey is back.']);
  });

  it('a failed result fails the task in its batch, and a report that names no task shows its text', () => {
    let s = play({ ...sequences.subagents, steps: sequences.subagents.steps.slice(0, 3) });
    const failed = { index: 0, id: 'bbbb2222', title: 'Check the build logs', model: 'Haiku 4.5', effort: 'medium', state: 'errored', error: 'The agent pod went away' };
    s = run(s, [
      ev('chat_message', { role: 'received', kind: 'subagent_result', content: 'Background task bbbb2222 (Check the build logs) FAILED.', ts: 50, attachments: [], row: { kind: 'task_result', ts: 50, tasks: [failed] } }),
      ev('chat_message', { role: 'received', kind: 'subagent_result', content: 'A report that names no task.', ts: 60, attachments: [], row: { kind: 'task_result', ts: 60, tasks: [], body_md: 'A report that names no task.' } }),
    ]);
    const batch = s.rows.find((r) => r.kind === 'subagents');
    expect(batch?.tasks?.map((t) => [t.id, t.state, t.error])).toEqual([
      ['aaaa1111', 'running', undefined],
      ['bbbb2222', 'errored', 'The agent pod went away'],
    ]);
    const results = s.rows.filter((r) => r.kind === 'task_result');
    expect(results.map((r) => [r.ts, r.tasks?.length, r.body_md])).toEqual([
      [50, 1, undefined],
      [60, 0, 'A report that names no task.'],
    ]);
  });

  it('keeps exactly one model and effort per task across progress updates', () => {
    const s = play({ ...sequences.subagents, steps: sequences.subagents.steps.slice(0, 5) });
    const t = s.rows[0]?.tasks?.[0];
    expect(t?.model).toBe('Sonnet 4.6');
    expect(t?.activity).toBe('running WebFetch');
  });

  it('holds waiting after done, says "1 task", and a turn event hands the label back to Thinking', () => {
    let s = run(start(), [ev('thinking', { step: 4, summary: 'Reticulating.' }), { type: 'thinking_promote' }, ev('done', { waiting_tasks: 2 })]);
    expect(s.running).toBe(false);
    expect(thinkingView(s)?.label).toBe('Waiting on 2 tasks');
    expect(canStop(s)).toBe(true);
    s = run(s, [{ type: 'waiting', tasks: 1 }]);
    expect(thinkingView(s)?.label).toBe('Waiting on 1 task');
    s = run(s, [ev('thinking', { step: 1 })]);
    expect(thinkingView(s)?.label).toBe('Thinking');
    expect(s.waitingTasks).toBe(0);
  });

  it('retires the dots when the batch finishes, and on an ordinary done or a missing count', () => {
    let s = run(start(), [ev('done', { waiting_tasks: 2 }), { type: 'waiting', tasks: 0 }]);
    expect(thinkingView(s)).toBeNull();
    expect(canStop(s)).toBe(false);
    s = run(start(), [ev('thinking', { step: 1 }), ev('done', { stop: 'end_turn' })]);
    expect(thinkingView(s)).toBeNull();
  });

  it('survives the 204 from a stream probe while waiting', () => {
    const s = run(start(goChat), [{ type: 'stream_open', at: AT }, { type: 'stream_closed' }]);
    expect(thinkingView(s)?.label).toBe('Waiting on 1 task');
  });
});

describe('messages and markers (chat-message-dedup, chat-turn-error-marker, chat-wake-update)', () => {
  it('paints a message from chat_message once, and closes the reply above it', () => {
    const s = play(sequences.fullTurn);
    expect(sent(s)).toHaveLength(1);
    expect(sent(s)[0]?.attachments?.[0]?.name).toBe('hosting-quote.pdf');
    expect(s.rows.filter((r) => r.kind === 'wake_update')).toHaveLength(1);
    expect(s.fill?.pct).toBe(64);
    expect(s.running).toBe(false);
    expect(s.reloadSeq).toBe(1);
  });

  it('paints the message the server emits, with its files (chat_message.json)', () => {
    const s = run(start(), [ev('chat_message', goChatMessage)]);
    expect(sent(s).map((r) => [r.ts, r.body_md])).toEqual([[goChatMessage.ts, goChatMessage.content]]);
    expect(sent(s)[0]?.attachments?.map((a) => a.name)).toEqual(goChatMessage.attachments.map((a) => a.name));
  });

  it('never paints a sent message from the POST alone', () => {
    let s = run(start(), [{ type: 'send_posted', text: 'Hi', response: { ts: 42 }, at: AT }]);
    expect(sent(s)).toHaveLength(0);
    expect(s.running).toBe(true);
    expect(s.awaiting).toEqual([42]);
    s = run(s, [ev('chat_message', { role: 'received', kind: 'direct_chat', content: 'Hi', ts: 42, attachments: [] })]);
    expect(sent(s)).toHaveLength(1);
    expect(s.awaiting).toEqual([]);
  });

  it('asks for a refetch when the stream closes before a sent message was painted', () => {
    const s = run(start(), [{ type: 'send_posted', text: 'Hi', response: { ts: 42 }, at: AT }, { type: 'stream_closed' }]);
    expect(s.reloadSeq).toBe(1);
    expect(s.running).toBe(false);
  });

  it('refetches when a connected stream ends mid-turn without done, never on a probe that did not connect', () => {
    const live = run(start(), [ev('delta', { text: 'Half', start_ts: 1 })]);
    // The browser reconnected to a hub that was gone: the turn's end was missed, the server has the rest.
    expect(run(live, [{ type: 'stream_closed', wasOpen: true }]).reloadSeq).toBe(1);
    // The 204 at page load (or from the org snapshot's reopen) says only "nothing to attach to".
    expect(run(live, [{ type: 'stream_closed', wasOpen: false }]).reloadSeq).toBe(0);
    expect(run(live, [{ type: 'stream_closed' }]).running).toBe(false);
  });

  it('a turn error is a marker below the partial reply; unknown kinds are ignored; the error event paints nothing and stops', () => {
    const s = play(sequences.turnError);
    expect(s.rows.map((r) => r.kind)).toEqual(['message', 'marker']);
    expect(s.rows[1]?.text).toBe('Stopped on an error');
    expect(s.rows[1]?.body_md).toBe('API Error: usage limit reached');
    expect(s.error).toBe("The model's usage limit was reached.");
    expect(s.running).toBe(false);
    expect(run(s, [{ type: 'dismiss_error' }]).error).toBeNull();
  });

  it('asks for a refetch when a rotation finishes, not before', () => {
    let s = run(start(), [ev('done', { waiting_tasks: 0, rotation: true })]);
    expect(s.rotating).toBe(true);
    expect(s.reloadSeq).toBe(0);
    s = run(s, [ev('rotation_done', {})]);
    expect(s.rotating).toBe(false);
    expect(s.reloadSeq).toBe(1);
  });
});

describe('a new chat (rotation)', () => {
  // A chat with something said in it, as the page holds it before New chat.
  const talked: Chat = { ...emptyChat, rows: goChat.rows.filter((r) => r.kind === 'message').slice(0, 2) };
  const fresh = freshChatAfter(talked);
  const REQUEST_TS = 1789923700000;
  const requests = (s: TranscriptState) => s.rows.filter((r) => r.kind === 'rotation_prompt');
  /** The recorded rotation up to, not including, the named event. */
  const until = (event: string): Sequence => {
    const steps = sequences.rotation.steps;
    return { ...sequences.rotation, steps: steps.slice(0, steps.findIndex((st) => 'event' in st && st.event === event)) };
  };

  it('the request shows at once on the page that made it: rotating, and the dots', () => {
    const s = run(start(talked), [{ type: 'rotation_requested', at: AT }]);
    expect(s.rotating).toBe(true);
    expect(canStop(s)).toBe(true);
    expect(thinkingView(s)?.label).toBe('Thinking');
    // The row comes with the stream's marker, which carries its ts.
    expect(requests(s)).toHaveLength(0);
  });

  it("the stream's marker paints the request once, as the row the GET serves for it", () => {
    const s = play(until('thinking'), start(talked));
    expect(s.rotating).toBe(true);
    expect(s.running).toBe(true);
    expect(requests(s).map((r) => [r.key, r.ts])).toEqual([['r:' + REQUEST_TS, REQUEST_TS]]);
    // The request lands below what was said before it.
    expect(s.rows.map((r) => r.kind)).toEqual(['message', 'message', 'rotation_prompt']);
    // A refetch mid-rotation brings the stored row: still one.
    const stored: Chat = { ...talked, running: true, rotating: true, rows: [...talked.rows, { kind: 'rotation_prompt', ts: REQUEST_TS, body_md: 'Reconcile your memory.' }] };
    const merged = run(s, [{ type: 'loaded', chat: stored, at: AT }]);
    expect(requests(merged)).toHaveLength(1);
    expect(merged.rotating).toBe(true);
  });

  it('a page that loaded mid-rotation takes the request from the GET, and the marker that follows adds nothing', () => {
    const stored: Chat = { ...talked, running: true, rotating: true, rows: [...talked.rows, { kind: 'rotation_prompt', ts: REQUEST_TS, body_md: 'Reconcile your memory.' }] };
    const s = play(until('thinking'), start(stored));
    expect(s.rotating).toBe(true);
    expect(requests(s)).toHaveLength(1);
  });

  it('follows the turn that folds the chat, and holds for rotation_done once it ends', () => {
    const s = play(until('rotation_done'), start(talked));
    expect(s.rows.map((r) => r.kind)).toEqual(['message', 'message', 'rotation_prompt', 'tool_use', 'message']);
    expect(s.rotating).toBe(true);
    expect(s.running).toBe(false);
    // The server still serves the chat that is on its way out: nothing is fetched yet.
    expect(s.reloadSeq).toBe(0);
    expect(s.rotationSeq).toBe(0);
  });

  it('rotation_done clears the page, and the fresh chat is all it shows, without the rotation turn', () => {
    let s = play(sequences.rotation, start(talked));
    expect(s.rows).toEqual([]);
    expect(s.loaded).toBe(false);
    expect(s.rotating).toBe(false);
    expect(s.running).toBe(false);
    expect(thinkingView(s)).toBeNull();
    expect(s.reloadSeq).toBe(1);
    expect(s.rotationSeq).toBe(1);
    s = run(s, [{ type: 'loaded', chat: fresh, at: AT }]);
    expect(s.loaded).toBe(true);
    expect(s.rows).toEqual([]);
    expect(s.generation).toBe(fresh.generation);
    expect(s.fill?.pct).toBe(0);
    // Counted once: the GET after rotation_done is not a second rotation.
    expect(s.rotationSeq).toBe(1);
  });

  it('a GET under another generation replaces the page even when rotation_done never arrived', () => {
    // The turn that folds the chat was followed live, then the connection was lost.
    let s = play(until('done'), start(talked));
    expect(s.rows.some((r) => r.live)).toBe(true);
    s = run(s, [{ type: 'stream_closed', wasOpen: true }]);
    expect(s.reloadSeq).toBe(1);
    s = run(s, [{ type: 'loaded', chat: fresh, at: AT }]);
    expect(s.rows).toEqual([]);
    expect(s.rotating).toBe(false);
    expect(s.running).toBe(false);
    expect(thinkingView(s)).toBeNull();
    expect(s.rotationSeq).toBe(1);
  });

  it('within one generation a refetch still keeps what this page painted ahead of it', () => {
    const live = run(start(talked), [ev('delta', { text: 'Half', start_ts: AT + 5 })]);
    const s = run(live, [{ type: 'loaded', chat: talked, at: AT }]);
    expect(replies(s).map((r) => r.body_md)).toContain('Half');
    expect(s.rotationSeq).toBe(0);
  });

  it('the first GET is never a rotation, whatever generation it names', () => {
    expect(start(fresh).rotationSeq).toBe(0);
    expect(start(talked).generation).toBe(talked.generation);
  });

  it('a message held during the rotation stays pending across it, and the fresh chat delivers it', () => {
    let s = run(play(until('done'), start(talked)), [ev('pending_message', goPendingMessage)]);
    s = run(s, [ev('done', { waiting_tasks: 0, rotation: true }), ev('rotation_done', {})]);
    expect(s.rows).toEqual([]);
    expect(s.pending.map((p) => p.id)).toEqual([goPendingMessage.id]);
    const delivered: Chat = {
      ...fresh,
      running: true,
      rows: [{ kind: 'message', ts: goPendingMessage.queued_at, role: 'sent', from: { kind: 'person', slug: 'ceo', name: 'You' }, body_md: goPendingMessage.text, attachments: [] }],
    };
    s = run(s, [{ type: 'loaded', chat: delivered, at: AT }]);
    expect(s.pending).toEqual([]);
    expect(sent(s).map((r) => r.body_md)).toEqual([goPendingMessage.text]);
    expect(s.running).toBe(true);
  });

  it('a pending message of a chat that rotated out of sight goes with it', () => {
    let s = run(start({ ...talked, running: true }), [ev('pending_message', goPendingMessage)]);
    s = run(s, [{ type: 'loaded', chat: fresh, at: AT }]);
    expect(s.pending).toEqual([]);
  });

  it('a late rotation_done is old news to a page already on the fresh chat', () => {
    // done held the stream; the connection dropped; the GET settled the page; the stream's fallback fires.
    let s = play(until('rotation_done'), start(talked));
    s = run(s, [{ type: 'stream_closed', wasOpen: true }, { type: 'loaded', chat: { ...fresh, running: true }, at: AT }]);
    s = run(s, [ev('delta', { text: 'Hello again', start_ts: AT + 9 })]);
    const after = run(s, [ev('rotation_done', {})]);
    expect(after).toBe(s);
  });

  it('a lost stream asks the server where the rotation stands, even one that never connected', () => {
    const asked = run(start(talked), [{ type: 'rotation_requested', at: AT }]);
    // The turn was over before the stream opened: a 204.
    expect(run(asked, [{ type: 'stream_closed', wasOpen: false }]).reloadSeq).toBe(1);
    const waiting = run(asked, [{ type: 'waiting', tasks: 2 }]);
    expect(run(waiting, [{ type: 'stream_closed', wasOpen: false }]).reloadSeq).toBe(1);
  });

  it('a turn that ends without a rotation ends the belief in one', () => {
    const s = run(start(talked), [{ type: 'rotation_requested', at: AT }, ev('done', { waiting_tasks: 0, rotation: false })]);
    expect(s.rotating).toBe(false);
    expect(s.reloadSeq).toBe(1);
  });

  it('a failed turn still rotates the chat: the error holds for rotation_done like done does', () => {
    let s = run(play(until('delta'), start(talked)), [ev('error', { error: 'The model could not be reached.', rotation: true })]);
    expect(s.error).toBe('The model could not be reached.');
    expect(s.rotating).toBe(true);
    expect(s.reloadSeq).toBe(0);
    s = run(s, [ev('rotation_done', {})]);
    expect(s.rows).toEqual([]);
    expect(s.reloadSeq).toBe(1);
    // What failed is still said on the fresh page, until dismissed.
    expect(s.error).toBe('The model could not be reached.');
    // Without a rotation an error is the end of it.
    const plain = run(start(talked), [ev('error', { error: 'Boom.', rotation: false })]);
    expect(plain.rotating).toBe(false);
    expect(plain.reloadSeq).toBe(1);
  });

  it('draws the request as one row of its own, never inside an agent turn', () => {
    const s = play(until('done'), start(talked));
    const blocks = toBlocks(s.rows, s.agent);
    expect(blocks.map((b) => (b.type === 'row' ? b.row.kind : 'agent'))).toEqual(['message', 'agent', 'rotation_prompt', 'agent', 'agent']);
  });
});

describe('pending messages', () => {
  it('paints the pending row from the POST and dedupes the echoing event', () => {
    const s = run(start(), [
      { type: 'send_posted', text: goPendingMessage.text, response: goMessagePost, at: AT },
      ev('pending_message', goPendingMessage),
    ]);
    expect(s.pending).toHaveLength(1);
    expect(s.pending[0]?.id).toBe(goPendingMessage.id);
    expect(sent(s)).toHaveLength(0);
  });

  it('delivers after the pause marker, below the partial reply, and pending_offered clears Delete', () => {
    const s = play(sequences.pendingSendNow);
    expect(s.rows.map((r) => (r.kind === 'marker' ? 'marker' : r.role))).toEqual(['received', 'marker', 'sent', 'received']);
    expect(s.rows[0]?.body_md).toBe('Asking Provider A about the invoice');
    expect(s.rows[2]?.ts).toBe(1789923601000);
    expect(s.pending).toHaveLength(0);
    const offered = play({ ...sequences.pendingSendNow, steps: sequences.pendingSendNow.steps.slice(0, 4) });
    expect(offered.pending[0]?.deletable).toBe(false);
  });

  it('deletes, and restores from the restore response', () => {
    let s = run(start(), [ev('pending_message', goPendingMessage), { type: 'pending_busy', id: goPendingMessage.id, op: 'deleting' }]);
    expect(s.pending[0]?.deleting).toBe(true);
    s = run(s, [{ type: 'pending_removed', id: goPendingMessage.id }, ev('pending_deleted', { id: goPendingMessage.id })]);
    expect(s.pending).toHaveLength(0);
    const { delivered_ts: _delivered, ...restored } = goPendingRestore;
    s = run(s, [{ type: 'pending_restored', message: restored }]);
    expect(s.pending).toHaveLength(1);
    // Restored straight into the transcript (the agent had gone idle): a refetch paints it.
    const direct = run(start(), [{ type: 'pending_restored', message: goPendingRestore }]);
    expect(direct.pending).toHaveLength(0);
    expect(direct.reloadSeq).toBe(1);
  });

  it('pending_delivered before chat_message moves the row, and the chat_message is a no-op', () => {
    const s = run(start(), [
      ev('pending_message', { ...goPendingMessage, queued_at: goPendingDelivered.ts }),
      ev('pending_delivered', goPendingDelivered),
      ev('chat_message', { role: 'received', kind: 'direct_chat', content: goPendingMessage.text, ts: goPendingDelivered.ts, attachments: [] }),
    ]);
    expect(sent(s)).toHaveLength(1);
    expect(sent(s)[0]?.attachments?.[0]?.url).toContain('/attachments/');
    expect(s.pending).toHaveLength(0);
  });
});

describe('a refetch racing a new turn', () => {
  const T0 = 1789923600000;
  const person = { kind: 'person' as const, slug: 'ceo', name: 'You' };
  const agent = { kind: 'agent' as const, slug: AGENT.slug, name: AGENT.name };

  it('keeps the rows the answer predates and its own view of the turn, then settles on the full copy', () => {
    let s = run(start(), [ev('chat_message', { kind: 'direct_chat', content: 'One', ts: T0 }), ev('delta', { text: 'Reply one', start_ts: T0 + 1000, model: 'Opus 5.5' }), ev('done', { waiting_tasks: 0 })]);
    expect(s.reloadSeq).toBe(1);
    // The refetch is in flight when the next turn starts.
    s = run(s, [
      ev('chat_message', { kind: 'direct_chat', content: 'Two', ts: T0 + 5000 }),
      ev('delta', { text: 'Reply two', start_ts: T0 + 6000, model: 'Opus 5.5' }),
      ev('tool_use_start', { tool_use_id: 'x', tool_name: 'file_view', start_ts: T0 + 7000 }),
      ev('tool_result', { tool_use_id: 'x', is_error: false, stdout: 'ok' }),
    ]);
    expect(s.running).toBe(true);
    // The answer predates the second turn; the first reply is stored with its flush time.
    const stale: Chat = {
      ...emptyChat,
      rows: [
        { kind: 'message', ts: T0, role: 'sent', from: person, body_md: 'One', attachments: [] },
        { kind: 'message', ts: T0 + 2000, role: 'received', from: agent, body_md: 'Reply one', attachments: [], model: 'Opus 5.5' },
      ],
    };
    s = reduceTranscript(s, { type: 'loaded', chat: stale, at: AT });
    expect(s.rows.map((r) => r.body_md ?? r.kind)).toEqual(['One', 'Reply one', 'Two', 'Reply two', 'tool_use']);
    expect(s.rows.filter((r) => r.body_md === 'Reply one')).toHaveLength(1);
    expect(s.running).toBe(true);
    expect(canStop(s)).toBe(true);
    // The refetch after this turn's done has everything: the server's copy is the only copy.
    s = run(s, [ev('done', { waiting_tasks: 0 })]);
    const full: Chat = {
      ...stale,
      rows: [
        ...stale.rows,
        { kind: 'message', ts: T0 + 5000, role: 'sent', from: person, body_md: 'Two', attachments: [] },
        { kind: 'message', ts: T0 + 6900, role: 'received', from: agent, body_md: 'Reply two', attachments: [], model: 'Opus 5.5' },
        { kind: 'tool_use', ts: T0 + 7000, tool_use_id: 'x', name: 'file_view', input: '{}', output: 'ok', status: 'done', started_ts: T0 + 7000, ended_ts: T0 + 7100 },
      ],
    };
    s = reduceTranscript(s, { type: 'loaded', chat: full, at: AT });
    expect(s.rows).toHaveLength(5);
    expect(s.rows.every((r) => !r.live)).toBe(true);
    expect(s.running).toBe(false);
  });

  it('a refetch after done that still says running keeps Stop until the reopened stream settles it (204)', () => {
    let s = run(start({ ...emptyChat, running: true }), [ev('done', { waiting_tasks: 0 })]);
    expect(s.reloadSeq).toBe(1);
    expect(canStop(s)).toBe(false);
    // The server clears its in-flight mark just after it emits done: the refetch can still say running.
    s = reduceTranscript(s, { type: 'loaded', chat: { ...emptyChat, running: true }, at: AT });
    // The answer may be right (a follow-up turn started), so Stop shows; the page reopens the stream.
    expect(canStop(s)).toBe(true);
    // Nothing is running by the time it connects: the server answers 204 and the probe never opens.
    s = reduceTranscript(s, { type: 'stream_closed', wasOpen: false });
    expect(s.running).toBe(false);
    expect(canStop(s)).toBe(false);
    expect(thinkingView(s)).toBeNull();
  });

  it('a refetch after done that says running is kept while the reopened stream shows a live turn', () => {
    let s = run(start(), [ev('done', { waiting_tasks: 0 })]);
    s = reduceTranscript(s, { type: 'loaded', chat: { ...emptyChat, running: true }, at: AT });
    expect(canStop(s)).toBe(true);
    s = run(s, [{ type: 'stream_open', at: AT }, ev('delta', { text: 'Reply two', start_ts: T0 + 5000, model: 'Opus 5.5' })]);
    expect(canStop(s)).toBe(true);
    s = run(s, [ev('done', { waiting_tasks: 0 })]);
    expect(canStop(s)).toBe(false);
  });

  it('keeps a pending message the answer predates, and drops one the answer shows delivered', () => {
    let s = run(start(), [ev('pending_message', goPendingMessage)]);
    s = reduceTranscript(s, { type: 'loaded', chat: emptyChat, at: AT });
    expect(s.pending.map((p) => p.id)).toEqual([goPendingMessage.id]);
    const delivered: Chat = { ...emptyChat, rows: [{ kind: 'message', ts: goPendingMessage.queued_at, role: 'sent', from: person, body_md: goPendingMessage.text, attachments: [] }] };
    s = reduceTranscript(s, { type: 'loaded', chat: delivered, at: AT });
    expect(s.pending).toHaveLength(0);
  });
});

describe('server rows', () => {
  it('marks a call listed as running as stale when no turn is, and a late result settles it', () => {
    const rows = mergeLoaded(goChat.rows, [], false);
    expect(rows.find((r) => r.tool_use_id === 'toolu_03')?.stale).toBe(true);
    expect(mergeLoaded(goChat.rows, [], true).find((r) => r.tool_use_id === 'toolu_03')?.stale).toBeUndefined();
    const s = run(start({ ...goChat, running: false, waiting_tasks: 0 }), [ev('tool_result', { tool_use_id: 'toolu_03', is_error: false, stdout: 'ok' })]);
    const row = s.rows.find((r) => r.tool_use_id === 'toolu_03');
    expect(row?.stale).toBe(false);
    expect(row?.status).toBe('done');
  });

  it('draws a background task transcript with the dispatching agent on the sent side (subagent_transcript.json)', () => {
    const rows = mergeLoaded((goSubagentTranscript as SubagentTranscript).rows, []);
    const party = { kind: 'agent' as const, slug: 'bbbb2222', name: 'Check the build logs' };
    const blocks = toBlocks(rows, party, { senderSlug: goSubagentTranscript.meta.parent });
    expect(blocks.map((b) => b.type)).toEqual(['row', 'agent']);
    expect(isSentSide(rows[0] as ChatRow, 'engineering-lead')).toBe(true);
    expect(isSentSide(rows[1] as ChatRow, 'engineering-lead')).toBe(false);
  });
});

describe('reconcileSnapshot (chat-thinking-reconcile, chat-waiting-tasks)', () => {
  const base = { agentState: 'idle', snapshotWaiting: 0, streamOpen: false, resumed: false, midTurnAtHide: false, running: false, waitingTasks: 0 };

  it('steady state: opens a missing stream for a working agent and never churns an open one', () => {
    expect(reconcileSnapshot({ ...base, agentState: 'running' })).toEqual({ open: 'if-closed' });
    expect(reconcileSnapshot({ ...base, agentState: 'running', streamOpen: true })).toEqual({});
    // No reload path in steady state: acting on "not working" would race the turn's own done.
    expect(reconcileSnapshot({ ...base, running: true })).toEqual({});
  });

  it('after a resume: force-reopens for a working agent, refetches a turn that ended, leaves a quiet one alone', () => {
    expect(reconcileSnapshot({ ...base, agentState: 'running', streamOpen: true, resumed: true })).toEqual({ open: 'force' });
    expect(reconcileSnapshot({ ...base, resumed: true, midTurnAtHide: true })).toEqual({ refetch: true });
    expect(reconcileSnapshot({ ...base, resumed: true, running: true })).toEqual({ refetch: true });
    expect(reconcileSnapshot({ ...base, resumed: true })).toEqual({});
    // Waiting on tasks is a steady state, not a finished turn.
    expect(reconcileSnapshot({ ...base, agentState: 'waiting', snapshotWaiting: 1, resumed: true, midTurnAtHide: true })).toEqual({ waiting: 1 });
    // An agent held after Stop is also "waiting", with nothing running: its turn did end while away.
    expect(reconcileSnapshot({ ...base, agentState: 'waiting', resumed: true, midTurnAtHide: true })).toEqual({ refetch: true });
  });

  it('sustains waiting from the count, follows it down, retires it when the batch finishes, and ignores a snapshot without this agent', () => {
    expect(reconcileSnapshot({ ...base, agentState: 'waiting', snapshotWaiting: 3, waitingTasks: 3 })).toEqual({ waiting: 3 });
    expect(reconcileSnapshot({ ...base, agentState: 'waiting', snapshotWaiting: 1, waitingTasks: 3 })).toEqual({ waiting: 1 });
    expect(reconcileSnapshot({ ...base, waitingTasks: 2 })).toEqual({ waiting: 0, refetch: true });
    // A live turn outranks waiting (chat-waiting-tasks: does not relabel to waiting while a turn is streaming).
    expect(reconcileSnapshot({ ...base, agentState: 'running', snapshotWaiting: 2, streamOpen: true, running: true, waitingTasks: 0 })).toEqual({});
    expect(reconcileSnapshot({ ...base, agentState: null, resumed: true, midTurnAtHide: true })).toEqual({});
  });

  it('shows nothing for an agent held after Stop: "waiting" with no tasks is not waiting on tasks', () => {
    expect(reconcileSnapshot({ ...base, agentState: 'waiting', snapshotWaiting: 0 })).toEqual({});
    const s = run(start(), [ev('done', { waiting_tasks: 0 })]);
    expect(thinkingView(s)).toBeNull();
    expect(canStop(s)).toBe(false);
  });

  it('refetches once when the batch finishes and not on the snapshots that follow (initiative-panel-live: no piled-up refetches)', () => {
    let s = run(start(), [ev('done', { waiting_tasks: 2 })]);
    const snapshot = (count: number) => {
      const d = reconcileSnapshot({ ...base, snapshotWaiting: count, waitingTasks: s.waitingTasks, running: s.running });
      if (d.waiting !== undefined) s = reduceTranscript(s, { type: 'waiting', tasks: d.waiting });
      return !!d.refetch;
    };
    expect(snapshot(2)).toBe(false);
    expect(thinkingView(s)?.label).toBe('Waiting on 2 tasks');
    expect(snapshot(1)).toBe(false);
    expect(thinkingView(s)?.label).toBe('Waiting on 1 task');
    expect(snapshot(0)).toBe(true);
    expect(thinkingView(s)).toBeNull();
    expect(snapshot(0)).toBe(false);
    expect(snapshot(0)).toBe(false);
  });
});

describe('scrolling', () => {
  it('counts rows that arrive while the person reads further up, and clears at the bottom', () => {
    let s = run(start(), [{ type: 'scrolled', atBottom: false }, ev('chat_message', { kind: 'direct_chat', content: 'a', ts: 1 }), ev('delta', { text: 'b', start_ts: 2 }), ev('delta', { text: 'c', start_ts: 2 })]);
    expect(s.unreadBelow).toBe(2);
    s = run(s, [{ type: 'scrolled', atBottom: true }]);
    expect(s.unreadBelow).toBe(0);
  });
});

describe('a cut tool call the stream finishes', () => {
  it('drops the cut flags once the events carry the call in full', () => {
    const chat: Chat = {
      ...emptyChat,
      running: true,
      rows: [{ kind: 'tool_use', ts: AT, tool_use_id: 'big', name: 'file_create', input: '{"path":"a.md","content":"x…"}', input_truncated: true, status: 'running', started_ts: AT }],
    };
    const s = run(start(chat), [
      { type: 'event', name: 'tool_use', data: { tool_use_id: 'big', tool_name: 'file_create', tool_input: '{"path":"a.md","content":"xyz"}' }, at: AT + 1 },
      { type: 'event', name: 'tool_result', data: { tool_use_id: 'big', stdout: 'wrote a.md' }, at: AT + 2 },
    ]);
    const row = s.rows.find((r) => r.tool_use_id === 'big');
    expect(row).toMatchObject({ input: '{"path":"a.md","content":"xyz"}', input_truncated: false, output: 'wrote a.md', output_truncated: false, status: 'done' });
  });
});
