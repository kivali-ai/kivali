// The agent chat transcript as a pure reducer: GET /api/v1/agents/{slug}/chat seeds it, the agent stream's
// events move it, and a few local actions (a send, a pending delete or restore) fill the gaps.
//
// Rules that shape everything below:
// - A message you sent is painted as delivered ONLY from a `chat_message` event or the GET rows. A send to a
//   busy agent is a pending row (from the POST's `pending_id` or the `pending_message` event) until
//   `pending_delivered` moves it into the transcript at the seam.
// - A background task's report is a message the agent received, and is painted where it landed like any
//   other: a `task_result` row, from the `chat_message` that carries it or from the GET rows. The batch
//   that dispatched the task shows the outcome as well.
// - Deltas are keyed by `start_ts`. The hub replays every delta of a message that has not been flushed yet
//   whenever a stream (re)opens, so a reopen marks the open reply as closed and the next delta for its
//   `start_ts` rebuilds it from scratch.
// - Running dots and the streaming caret never show on the same turn: `thinkingView` is hidden while any
//   reply shows its caret.
// - A new chat replaces the transcript, it never merges into it, and the page never reloads: the rows are
//   dropped on `rotation_done` and whenever the GET names another
//   `generation` than the one the rows were painted under (the event was missed: a hidden tab, a lost
//   connection). Nothing painted for a chat that is now a past chat survives either.

import type {
  AttachmentRef,
  Chat,
  ChatFill,
  ChatParty,
  MarkerKind,
  EffortOption,
  MessagePostResponse,
  ModelOption,
  PendingAttachment,
  PendingMessage,
  SubagentTask,
  SubagentTaskState,
  TranscriptRow,
} from '../api/types.gen';
import { isSubagentTool, pastTense, previewForTool, summarizeTool } from './toolSummary';

// ---- Types ----

/** A background task as the transcript shows it: the wire's task plus what it is doing right now. */
export interface LiveTask extends SubagentTask {
  activity?: string;
}

/** One transcript row: the wire's row plus the view's bookkeeping. */
export interface ChatRow extends Omit<TranscriptRow, 'tasks'> {
  /** Stable React key and dedupe key. */
  key: string;
  tasks?: LiveTask[];
  /** Built from stream events rather than read from the server. */
  live?: boolean;
  /** An agent reply still accepting deltas. */
  open?: boolean;
  /** An agent reply whose text is arriving right now: the honey caret. */
  caret?: boolean;
  /** A tool call's one-line summary. */
  summary?: string;
  /** A tool call's input as it streams in. */
  partialInput?: string;
  /** A tool call the server still lists as running although no turn is: its result was never recorded. */
  stale?: boolean;
}

/** A message you sent mid-turn that the agent has not read yet. */
export interface PendingView extends PendingMessage {
  /** Send now was clicked; waiting for the delivery. */
  sending?: boolean;
  /** Delete was clicked; waiting for the server. */
  deleting?: boolean;
}

export interface ThinkingState {
  /** Reasoning steps shown for the current interval (between two messages from the model). */
  steps: number;
  summary: string;
  /** A step not shown yet: it is promoted after a short debounce unless a message lands first. */
  pendingStep: number;
  pendingSummary: string;
  /** When the current interval started (unix ms); the elapsed clock counts from it. 0 when unknown. */
  since: number;
}

export interface ChatSettingsView {
  models: ModelOption[];
  currentModel: string;
  currentEffort: string;
}

/** The efforts the current model offers, from its row in `models`; empty when it offers none or is not listed. */
export function currentEfforts(settings: ChatSettingsView): EffortOption[] {
  return settings.models.find((m) => m.id === settings.currentModel)?.efforts ?? [];
}

/** The effort the chip shows: the current one when the model offers it, else the model's default. */
export function selectedEffort(settings: ChatSettingsView): string {
  const efforts = currentEfforts(settings);
  if (efforts.some((e) => e.id === settings.currentEffort)) return settings.currentEffort;
  return efforts.find((e) => e.default)?.id ?? efforts[0]?.id ?? '';
}

export interface TranscriptState {
  agent: ChatParty;
  loaded: boolean;
  rows: ChatRow[];
  pending: PendingView[];
  /** A turn is running: Stop shows, and the dots show unless text is arriving. */
  running: boolean;
  thinking: ThinkingState;
  /** Background tasks still running after the turn ended ("Waiting on 2 tasks"). */
  waitingTasks: number;
  fill: ChatFill | null;
  settings: ChatSettingsView;
  archived: boolean;
  /** The stream's fatal error, shown as a banner until dismissed. */
  error: string | null;
  /**
   * A new chat was asked for and this one is on its way out: the agent is folding it into memory, or that
   * turn has ended and the chat is being archived. `rotation_done` follows.
   */
  rotating: boolean;
  /** The chat the rows belong to (the GET's `generation`); null until a GET has said. */
  generation: string | null;
  /** Bumped when a new chat replaced the one on the page, so what counts past chats can be read again. */
  rotationSeq: number;
  /** Bumped whenever the chat should be fetched again (turn end, rotation done, a lost send). */
  reloadSeq: number;
  /** POSTed messages (their ts) that went straight to an idle agent and have not been painted yet. */
  awaiting: number[];
  /** Whether the person is reading the bottom of the transcript. */
  atBottom: boolean;
  /** Rows that arrived while the person was scrolled up. */
  unreadBelow: number;
}

export type TranscriptAction =
  | { type: 'loaded'; chat: Chat; at: number }
  | { type: 'event'; name: string; data: unknown; at: number }
  | { type: 'stream_open'; at: number }
  /** The connection ended without done/error. `wasOpen` says it had connected (a drop), not a 204 probe. */
  | { type: 'stream_closed'; wasOpen?: boolean }
  | { type: 'thinking_promote' }
  | { type: 'send_posted'; text: string; response: MessagePostResponse; at: number }
  /** New chat was confirmed on this page and the server started the rotation. */
  | { type: 'rotation_requested'; at: number }
  | { type: 'pending_busy'; id: string; op: 'sending' | 'deleting' | null }
  | { type: 'pending_removed'; id: string }
  | { type: 'pending_restored'; message: PendingMessage & { delivered_ts?: number } }
  | { type: 'waiting'; tasks: number }
  | { type: 'settings'; currentModel: string; currentEffort: string }
  | { type: 'scrolled'; atBottom: boolean }
  | { type: 'dismiss_error' };

// ---- Constants ----

/** The words each stored marker kind shows; mirrors markerText in internal/web/transcript.go. */
export const MARKER_TEXT: Record<string, string> = {
  'user-interruption': 'Chat interrupted by Stop',
  'runtime-disruption': 'Interrupted by a runtime restart',
  'turn-error': 'Stopped on an error',
  'paused-to-deliver': 'Paused to deliver your message',
};

/** The stream's marker for a new chat that was asked for. It is painted as the `rotation_prompt` row it stands for. */
const ROTATION_MARKER = 'rotation';

/** How long a reasoning step waits before it is shown. */
export const THINKING_PROMOTE_MS = 300;

const YOU: ChatParty = { kind: 'person', slug: 'ceo', name: 'You' };

const emptyThinking: ThinkingState = { steps: 0, summary: '', pendingStep: 0, pendingSummary: '', since: 0 };

export function initialTranscript(agent: { slug: string; name: string }): TranscriptState {
  return {
    agent: { kind: 'agent', slug: agent.slug, name: agent.name },
    loaded: false,
    rows: [],
    pending: [],
    running: false,
    thinking: emptyThinking,
    waitingTasks: 0,
    fill: null,
    settings: { models: [], currentModel: '', currentEffort: '' },
    archived: false,
    error: null,
    rotating: false,
    generation: null,
    rotationSeq: 0,
    reloadSeq: 0,
    awaiting: [],
    atBottom: true,
    unreadBelow: 0,
  };
}

// ---- Small helpers ----

type Obj = Record<string, unknown>;

function obj(v: unknown): Obj {
  return v && typeof v === 'object' && !Array.isArray(v) ? (v as Obj) : {};
}
function num(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}
function text(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

/** The download link for a stored file. */
export function attachmentUrl(sha: string): string {
  return '/attachments/' + encodeURIComponent(sha);
}

/** A pending attachment ({sha, name}) as a transcript attachment. The size is unknown until the GET. */
export function toAttachmentRef(a: PendingAttachment): AttachmentRef {
  return { name: a.name || 'attachment', size_bytes: 0, url: attachmentUrl(a.sha) };
}

function wireAttachments(v: unknown): AttachmentRef[] {
  if (!Array.isArray(v)) return [];
  return v.map((a) => {
    const o = obj(a);
    return toAttachmentRef({ sha: text(o.sha), name: text(o.name) });
  });
}

/** The dedupe key for a row read from the server. */
export function rowKey(r: TranscriptRow, index: number): string {
  switch (r.kind) {
    case 'message':
      return 'm:' + (r.role ?? '') + ':' + r.ts;
    case 'tool_use':
      return r.tool_use_id ? 't:' + r.tool_use_id : 't@' + r.ts + ':' + index;
    case 'subagents':
      return r.tool_use_id ? 's:' + r.tool_use_id : 's@' + r.ts + ':' + index;
    case 'task_result':
      return taskResultKey(r.ts);
    case 'marker':
      return 'k:' + (r.marker_kind ?? '') + ':' + r.ts;
    case 'doc_published':
      return r.tool_use_id ? 'd:' + r.tool_use_id : 'd@' + r.ts + ':' + index;
    case 'rotation_prompt':
      return rotationKey(r.ts);
    default:
      return r.kind + ':' + r.ts + ':' + index;
  }
}

/** One key for the request's row whether the GET or the stream's marker brought it: both carry the prompt's ts. */
const rotationKey = (ts: number) => 'r:' + ts;
/** One key for a task's result whether the GET or the stream's chat_message brought it: both carry the report's ts. */
const taskResultKey = (ts: number) => 'tr:' + ts;
const sentKey = (ts: number) => 'm:sent:' + ts;
const replyKey = (startTs: number) => 'a:' + startTs;

function hasKey(rows: readonly ChatRow[], key: string): boolean {
  return rows.some((r) => r.key === key);
}

function withRows(state: TranscriptState, rows: ChatRow[], appended: number): TranscriptState {
  return { ...state, rows, unreadBelow: state.atBottom ? 0 : state.unreadBelow + appended };
}

function append(state: TranscriptState, row: ChatRow): TranscriptState {
  const visible = row.kind !== 'wake_update';
  return withRows(state, [...state.rows, row], visible ? 1 : 0);
}

function replaceRow(state: TranscriptState, key: string, fn: (r: ChatRow) => ChatRow): TranscriptState {
  let changed = false;
  const rows = state.rows.map((r) => {
    if (r.key !== key) return r;
    changed = true;
    return fn(r);
  });
  return changed ? { ...state, rows } : state;
}

/** Closes the reply that is accepting deltas (a tool call, a message or a marker came in after it). */
function closeReply(state: TranscriptState): TranscriptState {
  if (!state.rows.some((r) => r.open || r.caret)) return state;
  return { ...state, rows: state.rows.map((r) => (r.open || r.caret ? { ...r, open: false, caret: false } : r)) };
}

/** Hides the caret without closing the reply: reasoning between two text blocks of one buffered reply. */
function hideCaret(state: TranscriptState): TranscriptState {
  if (!state.rows.some((r) => r.caret)) return state;
  return { ...state, rows: state.rows.map((r) => (r.caret ? { ...r, caret: false } : r)) };
}

/** Any turn event proves a turn is running, and reasoning takes the label back from waiting. */
function turnEvent(state: TranscriptState): TranscriptState {
  return state.running && state.waitingTasks === 0 ? state : { ...state, running: true, waitingTasks: 0 };
}

/** A message came back from the model: the reasoning interval ends. */
function resetInterval(state: TranscriptState, since: number): TranscriptState {
  return { ...state, thinking: { ...emptyThinking, since } };
}

// ---- Loading ----

function isFinishedLocal(r: ChatRow): boolean {
  if (r.kind === 'tool_use') return r.status !== 'running';
  if (r.kind === 'subagents') return (r.tasks ?? []).every((t) => t.state !== 'running');
  return true;
}

/**
 * Server rows, plus the live rows the server has not written yet: the reply still streaming, tool calls
 * and task batches the GET does not know about, and anything newer than the server's last row. Everything
 * else local is dropped: the server's copy wins (a flushed reply is stamped with its flush time, not its
 * start_ts, so it cannot be matched by key; it is older than the server's copy and goes).
 *
 * `ahead` is true when this page holds rows newer than the answer: the GET went out before a turn that has
 * since started (its events arrived while the fetch was in flight), so the answer says nothing about the
 * turn's state.
 */
function mergeRows(serverRows: readonly TranscriptRow[], local: readonly ChatRow[], turnRunning: boolean): { rows: ChatRow[]; ahead: boolean } {
  const rows: ChatRow[] = serverRows.map((r, i) => {
    const row: ChatRow = { ...r, key: rowKey(r, i) };
    // A call still "running" with no turn in flight never got its result written (a crash, or unknown): it
    // must not animate. While a turn runs, its tool_result event settles the row by tool_use_id.
    if (!turnRunning && r.kind === 'tool_use' && r.status === 'running') row.stale = true;
    return row;
  });
  const keys = new Set(rows.map((r) => r.key));
  const newest = rows.reduce((m, r) => Math.max(m, r.ts), 0);
  let ahead = false;
  for (const r of local) {
    if (!r.live || keys.has(r.key)) continue;
    if (r.ts > newest) {
      // Painted from events the answer predates: the answer cannot replace these.
      rows.push(r);
      ahead = true;
    } else if (r.kind === 'message' && r.open) {
      rows.push(r);
    } else if ((r.kind === 'tool_use' || r.kind === 'subagents') && !isFinishedLocal(r)) {
      rows.push(r);
    }
  }
  return { rows, ahead };
}

export function mergeLoaded(serverRows: readonly TranscriptRow[], local: readonly ChatRow[], turnRunning = false): ChatRow[] {
  return mergeRows(serverRows, local, turnRunning).rows;
}

/**
 * A refetch is how the view settles on the server's copy: a flushed reply's stored ts is its flush time,
 * not its start_ts, so live and stored rows cannot be matched by key; a running call whose result never
 * came must stop animating; and `pending_delivered` only moves the row, the GET fills in what the fold
 * added (attachment sizes, the row the follow-up turn wrote first). The rules above keep a refetch from
 * clobbering a turn that started while it was in flight.
 *
 * They hold within one chat. An answer that names another generation is a new chat: the rows and pending
 * messages this page holds belong to a past chat, and none of them is "ahead" of anything. Without this
 * an emptied chat kept every live row of the turn that folded it, since they are all newer than nothing.
 */
function loaded(state: TranscriptState, chat: Chat, at: number): TranscriptState {
  const rotated = state.generation !== null && state.generation !== chat.generation;
  const mine = rotated ? { ...state, rows: [], pending: [], thinking: emptyThinking, running: false } : state;
  const { rows, ahead } = mergeRows(chat.rows, mine.rows, chat.running);
  const local = new Map(mine.pending.map((p) => [p.id, p]));
  const pending: PendingView[] = chat.pending.map((p) => {
    const was = local.get(p.id);
    return was ? { ...p, sending: was.sending, deleting: was.deleting } : p;
  });
  const painted = new Set(rows.filter((r) => r.kind === 'message').map((r) => r.ts));
  // A pending message the answer does not list was staged after the GET went out, unless the answer shows
  // it delivered. The stream retires it (pending_delivered, pending_deleted); the answer cannot.
  const listed = new Set(pending.map((p) => p.id));
  for (const p of mine.pending) {
    if (!listed.has(p.id) && !painted.has(p.queued_at)) pending.push(p);
  }
  const streaming = rows.some((r) => r.open);
  // `running` covers a live turn and background tasks alike. With tasks outstanding the view starts in
  // waiting mode; the first turn event, if a turn is live, takes the label back.
  // When this page is ahead of the answer, its own view of the turn stands.
  const waiting = streaming ? 0 : ahead ? mine.waitingTasks : chat.waiting_tasks;
  const running = streaming || (ahead ? mine.running : chat.running && waiting === 0);
  return {
    ...state,
    loaded: true,
    rows,
    pending,
    running,
    waitingTasks: waiting,
    thinking: mine.thinking.since ? mine.thinking : { ...emptyThinking, since: running || waiting > 0 ? at : 0 },
    fill: chat.fill,
    settings: {
      models: chat.models,
      currentModel: chat.current_model,
      currentEffort: chat.current_effort,
    },
    archived: chat.archived,
    rotating: chat.rotating,
    generation: chat.generation,
    rotationSeq: rotated ? state.rotationSeq + 1 : state.rotationSeq,
    awaiting: state.awaiting.filter((ts) => !painted.has(ts)),
    unreadBelow: 0,
  };
}

// ---- Stream events ----

function onDelta(state: TranscriptState, d: Obj, at: number): TranscriptState {
  let s = turnEvent(state);
  const startTs = num(d.start_ts) ?? at;
  const model = text(d.model);
  const effort = text(d.effort);
  const chunk = text(d.text);
  let open = s.rows.find((r) => r.open);
  // A fresh text block (no reply open) ends the reasoning interval.
  if (!open) s = resetInterval(s, startTs);
  // The answering model changed mid-turn: the server flushed its text as its own row, so split here too.
  if (open && model && open.model && open.model !== model) {
    s = closeReply(s);
    open = undefined;
  }
  if (open) {
    const key = open.key;
    return replaceRow(s, key, (r) => ({ ...r, body_md: (r.body_md ?? '') + chunk, caret: true }));
  }
  const key = replyKey(startTs);
  if (hasKey(s.rows, key)) {
    // Replay after a reopen: adopt the reply this page painted before and rebuild it from the first delta.
    return replaceRow(s, key, (r) => ({ ...r, body_md: chunk, open: true, caret: true, model: model || r.model, effort: effort || r.effort }));
  }
  const row: ChatRow = {
    key,
    kind: 'message',
    ts: startTs,
    role: 'received',
    from: s.agent,
    body_md: chunk,
    attachments: [],
    live: true,
    open: true,
    caret: true,
  };
  if (model) row.model = model;
  if (effort) row.effort = effort;
  return append(s, row);
}

function onThinking(state: TranscriptState, d: Obj): TranscriptState {
  const s = hideCaret(turnEvent(state));
  const step = num(d.step) ?? 0;
  const summary = text(d.summary);
  const t = s.thinking;
  return {
    ...s,
    thinking: {
      ...t,
      pendingStep: Math.max(t.pendingStep, step),
      pendingSummary: summary || t.pendingSummary,
    },
  };
}

function promote(state: TranscriptState): TranscriptState {
  const t = state.thinking;
  if (!t.pendingStep && !t.pendingSummary) return state;
  return {
    ...state,
    thinking: {
      ...t,
      steps: Math.max(t.steps, t.pendingStep),
      summary: t.pendingSummary || t.summary,
      pendingStep: 0,
      pendingSummary: '',
    },
  };
}

function onToolUseStart(state: TranscriptState, d: Obj, at: number): TranscriptState {
  const startTs = num(d.start_ts) ?? at;
  let s = closeReply(resetInterval(turnEvent(state), startTs));
  const id = text(d.tool_use_id);
  const name = text(d.tool_name);
  if (!id) return s;
  if (hasKey(s.rows, 't:' + id) || hasKey(s.rows, 's:' + id) || hasKey(s.rows, 'd:' + id) || hasKey(s.rows, 'f:' + id)) return s;
  if (isSubagentTool(name)) {
    s = append(s, { key: 's:' + id, kind: 'subagents', ts: startTs, tool_use_id: id, tasks: [], live: true });
    return s;
  }
  return append(s, {
    key: 't:' + id,
    kind: 'tool_use',
    ts: startTs,
    tool_use_id: id,
    name,
    status: 'running',
    started_ts: startTs,
    partialInput: '',
    summary: '',
    live: true,
  });
}

function onToolInputDelta(state: TranscriptState, d: Obj): TranscriptState {
  const s = turnEvent(state);
  const key = 't:' + text(d.tool_use_id);
  return replaceRow(s, key, (r) => {
    if (r.status !== 'running') return r;
    const partial = (r.partialInput ?? '') + text(d.text);
    const name = r.name ?? '';
    return { ...r, partialInput: partial, input: previewForTool(name, partial), summary: summarizeTool(name, partial) || r.summary || '' };
  });
}

function onToolUse(state: TranscriptState, d: Obj): TranscriptState {
  const s = turnEvent(state);
  const key = 't:' + text(d.tool_use_id);
  return replaceRow(s, key, (r) => {
    if (r.status !== 'running') return r;
    const name = text(d.tool_name) || r.name || '';
    const input = text(d.tool_input);
    return { ...r, name, input, input_truncated: false, summary: summarizeTool(name, input) || r.summary || '' };
  });
}

function finishTool(r: ChatRow, status: 'done' | 'error', at: number): ChatRow {
  return { ...r, status, stale: false, ended_ts: r.ended_ts ?? at, summary: r.summary ? pastTense(r.summary) : '' };
}

function onToolResult(state: TranscriptState, d: Obj, at: number): TranscriptState {
  const s = turnEvent(state);
  const key = 't:' + text(d.tool_use_id);
  const isError = d.is_error === true;
  return replaceRow(s, key, (r) => {
    if (r.status !== 'running') return r;
    const out = text(d.stdout) || text(d.error);
    const next = finishTool(r, isError ? 'error' : 'done', at);
    if (isError) next.is_error = true;
    if (out) {
      next.output = out;
      next.output_truncated = false;
    }
    return next;
  });
}

function onDocPublished(state: TranscriptState, d: Obj, at: number): TranscriptState {
  const s = turnEvent(state);
  const id = text(d.tool_use_id);
  if (!id || hasKey(s.rows, 'd:' + id)) return s;
  const path = text(d.doc_path);
  // The server sends the recipients as the publish call wrote them: one string, slugs separated by commas.
  const slugs = Array.isArray(d.doc_to) ? d.doc_to.map((v) => text(v)) : text(d.doc_to).split(',');
  const to = slugs.map((v) => v.trim()).filter((v) => v !== '').map((v) => ({ slug: v, name: v }));
  const row: ChatRow = {
    key: 'd:' + id,
    kind: 'doc_published',
    ts: at,
    tool_use_id: id,
    title: text(d.doc_title),
    message_type: text(d.doc_type),
    to,
    attachments: wireAttachments(d.doc_files),
    live: true,
  };
  // The stored markdown file: a raw link inside the card, never the row's own link.
  if (path) row.raw_url = path.startsWith('/') ? path : '/' + path;
  if (typeof d.doc_body === 'string') row.body_md = d.doc_body;
  const assignment = num(d.doc_assignment);
  if (assignment !== undefined) row.assignment_ref = { id: assignment, title: '' };
  // The publish call's own chip becomes the published document, as the GET renders it.
  if (hasKey(s.rows, 't:' + id)) {
    return { ...s, rows: s.rows.map((r) => (r.key === 't:' + id ? { ...row, ts: r.ts } : r)) };
  }
  return append(s, row);
}

function onFileShared(state: TranscriptState, d: Obj, at: number): TranscriptState {
  const s = closeReply(turnEvent(state));
  const id = text(d.tool_use_id);
  const files = wireAttachments(d.files);
  if (files.length === 0) return s;
  const key = 'f:' + (id || String(at));
  if (hasKey(s.rows, key)) return s;
  const caption = text(d.caption);
  const body = caption || (files.length === 1 ? 'Shared file: ' + (files[0]?.name ?? 'attachment') : 'Shared ' + files.length + ' files');
  const row: ChatRow = { key, kind: 'file_shared', ts: at, from: s.agent, body_md: body, attachments: files, live: true };
  if (id) row.tool_use_id = id;
  if (id && hasKey(s.rows, 't:' + id)) {
    return { ...s, rows: s.rows.map((r) => (r.key === 't:' + id ? { ...row, ts: r.ts } : r)) };
  }
  return append(s, row);
}

// ---- Background tasks ----

/** The batch a task event belongs to: by tool_use_id, else the newest batch still running. */
function findBatch(rows: readonly ChatRow[], toolUseId: string): ChatRow | undefined {
  if (toolUseId) {
    const hit = rows.find((r) => r.kind === 'subagents' && r.tool_use_id === toolUseId);
    if (hit) return hit;
  }
  for (let i = rows.length - 1; i >= 0; i--) {
    const r = rows[i];
    if (r?.kind === 'subagents' && (r.tasks ?? []).some((t) => t.state === 'running')) return r;
  }
  return undefined;
}

function taskState(status: string): SubagentTaskState | undefined {
  switch (status) {
    case 'completed':
    case 'done':
      return 'done';
    case 'errored':
      return 'errored';
    case 'running':
    case 'queued':
      return 'running';
    default:
      return undefined;
  }
}

function upsertTask(tasks: readonly LiveTask[], index: number, patch: Partial<LiveTask>): LiveTask[] {
  const at = tasks.findIndex((t) => t.index === index);
  if (at < 0) {
    const t: LiveTask = { index, id: '', title: '', model: '', effort: '', state: 'running', ...patch };
    return [...tasks, t].sort((a, b) => a.index - b.index);
  }
  return tasks.map((t, i) => (i === at ? { ...t, ...patch } : t));
}

function sessionPatch(id: string): Partial<LiveTask> {
  return id ? { id } : {};
}

function withBatch(state: TranscriptState, toolUseId: string, at: number, fn: (tasks: LiveTask[]) => LiveTask[]): TranscriptState {
  const batch = findBatch(state.rows, toolUseId);
  if (batch) return replaceRow(state, batch.key, (r) => ({ ...r, tasks: fn(r.tasks ?? []) }));
  if (!toolUseId) return state;
  // The batch's tool call has not reached this page (a reopen that replayed only the task events).
  return append(state, { key: 's:' + toolUseId, kind: 'subagents', ts: at, tool_use_id: toolUseId, tasks: fn([]), live: true });
}

function onSubagentStarted(state: TranscriptState, d: Obj, at: number): TranscriptState {
  const tasks = Array.isArray(d.tasks) ? d.tasks.map(obj) : [];
  return withBatch(state, text(d.tool_use_id), at, (list) => {
    let out = list;
    for (const t of tasks) {
      const index = num(t.index) ?? out.length;
      const patch: Partial<LiveTask> = { ...sessionPatch(text(t.id)), title: text(t.description) };
      if (text(t.model)) patch.model = text(t.model);
      if (text(t.effort)) patch.effort = text(t.effort);
      out = upsertTask(out, index, patch);
    }
    return out;
  });
}

function onSubagentProgress(state: TranscriptState, d: Obj, at: number): TranscriptState {
  const index = num(d.index);
  if (index === undefined) return state;
  const st = taskState(text(d.status));
  return withBatch(state, text(d.tool_use_id), at, (list) => {
    const patch: Partial<LiveTask> = sessionPatch(text(d.id));
    if (st) patch.state = st;
    if (st === 'done' && text(d.final_text)) patch.output_md = text(d.final_text);
    if (st === 'errored') patch.error = text(d.error) || 'It stopped without saying why.';
    if (st !== 'running') patch.activity = '';
    return upsertTask(list, index, patch);
  });
}

function onSubagentActivity(state: TranscriptState, d: Obj, at: number): TranscriptState {
  const index = num(d.index);
  if (index === undefined) return state;
  return withBatch(state, text(d.tool_use_id), at, (list) => {
    const current = list.find((t) => t.index === index);
    // Only a running task shows what it is doing; a finished one keeps its result.
    if (current && current.state !== 'running') return list;
    return upsertTask(list, index, { ...sessionPatch(text(d.id)), activity: text(d.current_activity) });
  });
}

function onSubagentCompleted(state: TranscriptState, d: Obj, at: number): TranscriptState {
  const tasks = Array.isArray(d.tasks) ? d.tasks.map(obj) : [];
  return withBatch(state, text(d.tool_use_id), at, (list) => {
    let out = list;
    for (const t of tasks) {
      const index = num(t.index);
      if (index === undefined) continue;
      const err = text(t.error);
      const patch: Partial<LiveTask> = { ...sessionPatch(text(t.id)), state: err ? 'errored' : 'done', activity: '' };
      if (err) patch.error = err;
      else if (text(t.final_text)) patch.output_md = text(t.final_text);
      out = upsertTask(out, index, patch);
    }
    return out;
  });
}

/** A task as a result row carries it: the wire's SubagentTask. */
function wireTask(v: unknown): LiveTask {
  const o = obj(v);
  const t: LiveTask = { index: num(o.index) ?? 0, id: text(o.id), title: text(o.title), model: text(o.model), effort: text(o.effort), state: taskState(text(o.state)) ?? 'done' };
  if (text(o.error)) t.error = text(o.error);
  if (text(o.output_md)) t.output_md = text(o.output_md);
  if (text(o.url)) t.url = text(o.url);
  return t;
}

/**
 * The task a result reports on is over, in the batch that dispatched it too: a task still shown as running
 * there takes the outcome the report brought. The report names its task by id, in its row or in its text.
 */
function foldTaskResult(state: TranscriptState, content: string, reported: LiveTask | undefined): TranscriptState {
  let s = state;
  for (const r of state.rows) {
    if (r.kind !== 'subagents') continue;
    for (const t of r.tasks ?? []) {
      if (!t.id || t.state !== 'running') continue;
      if (reported?.id ? t.id !== reported.id : !content.includes(t.id)) continue;
      const patch: Partial<LiveTask> = { state: reported?.state === 'errored' ? 'errored' : 'done', activity: '' };
      if (reported?.output_md) patch.output_md = reported.output_md;
      if (reported?.error) patch.error = reported.error;
      s = replaceRow(s, r.key, (row) => ({ ...row, tasks: upsertTask(row.tasks ?? [], t.index, patch) }));
    }
  }
  return s;
}

/**
 * A finished task's report reached the agent. It is painted where it landed, as the `task_result` row the
 * GET serves for the same ts, so whichever arrives second finds it there.
 */
function onTaskResult(state: TranscriptState, d: Obj): TranscriptState {
  const row = obj(d.row);
  const tasks = Array.isArray(row.tasks) ? row.tasks.map(wireTask) : [];
  const s = foldTaskResult(state, text(d.content), tasks[0]);
  const ts = num(row.ts);
  if (row.kind !== 'task_result' || ts === undefined || hasKey(s.rows, taskResultKey(ts))) return s;
  const result: ChatRow = { key: taskResultKey(ts), kind: 'task_result', ts, tasks, live: true };
  if (text(row.body_md)) result.body_md = text(row.body_md);
  // The report lands below any text the agent had written when it arrived.
  return append(closeReply(s), result);
}

// ---- Messages, markers, pending ----

function sentRow(ts: number, body: string, attachments: AttachmentRef[], live: boolean): ChatRow {
  return { key: sentKey(ts), kind: 'message', ts, role: 'sent', from: YOU, body_md: body, attachments, pending: false, live };
}

function onChatMessage(state: TranscriptState, d: Obj): TranscriptState {
  let s = turnEvent(state);
  const kind = text(d.kind) || 'direct_chat';
  const content = text(d.content);
  if (kind === 'subagent_result') return onTaskResult(s, d);
  const ts = num(d.ts);
  if (ts === undefined) return s;
  s = { ...s, awaiting: s.awaiting.filter((t) => t !== ts), pending: s.pending.filter((p) => p.queued_at !== ts) };
  if (s.rows.some((r) => r.kind === 'message' && r.role === 'sent' && r.ts === ts)) return s;
  s = closeReply(s);
  return append(s, sentRow(ts, content, wireAttachments(d.attachments), true));
}

/**
 * A new chat was asked for: the turn now starting folds this chat into memory, and the chat is archived
 * when it ends. The row is the `rotation_prompt` the GET serves for the same ts, so whichever arrives
 * second finds it there.
 */
function onRotationMarker(state: TranscriptState, d: Obj): TranscriptState {
  const s = { ...turnEvent(state), rotating: true };
  const ts = num(d.ts);
  if (ts === undefined || hasKey(s.rows, rotationKey(ts))) return s;
  return append(closeReply(s), { key: rotationKey(ts), kind: 'rotation_prompt', ts, live: true });
}

function onChatMarker(state: TranscriptState, d: Obj): TranscriptState {
  const kind = text(d.kind);
  if (kind === ROTATION_MARKER) return onRotationMarker(state, d);
  const label = MARKER_TEXT[kind];
  if (!label) return state;
  const ts = num(d.ts) ?? 0;
  const key = 'k:' + kind + ':' + ts;
  if (hasKey(state.rows, key)) return state;
  // The marker lands below any text the stopped turn had already written.
  const s = closeReply(state);
  return append(s, { key, kind: 'marker', ts, marker_kind: kind as MarkerKind, text: label, body_md: text(d.content), live: true });
}

function onWakeUpdate(state: TranscriptState, d: Obj, at: number): TranscriptState {
  const ts = num(d.ts) ?? at;
  if (state.rows.some((r) => r.kind === 'wake_update' && r.ts === ts)) return state;
  const s = closeReply(state);
  return append(s, { key: 'w:' + ts, kind: 'wake_update', ts, body_md: text(d.content), live: true });
}

function onChatFill(state: TranscriptState, d: Obj): TranscriptState {
  const pct = num(d.chat_fill_pct);
  if (pct === undefined) return state;
  const prev = state.fill;
  return {
    ...state,
    fill: {
      pct: Math.max(0, Math.min(100, pct)),
      tokens: num(d.chat_token_est) ?? prev?.tokens ?? 0,
      limit: num(d.chat_context_limit) ?? prev?.limit ?? 0,
      bucket: text(d.chat_fill_bucket) || prev?.bucket || '',
      long_threshold: num(d.chat_long_thresh) ?? prev?.long_threshold ?? 0,
      resolved_model: text(d.chat_resolved_model) || prev?.resolved_model || '',
    },
  };
}

function endTurn(state: TranscriptState, waiting: number): TranscriptState {
  const s = closeReply(state);
  return { ...s, running: false, waitingTasks: waiting, thinking: waiting > 0 ? { ...emptyThinking, since: s.thinking.since } : emptyThinking };
}

// A turn that ends with `rotation: true` is followed by `rotation_done`, once the chat is archived. Until
// then the server still serves the chat that is on its way out, so nothing is fetched. Without the flag
// no rotation is under way, whatever this page believed.
function onDone(state: TranscriptState, d: Obj): TranscriptState {
  const waiting = Math.max(0, num(d.waiting_tasks) ?? 0);
  const s = endTurn(state, waiting);
  if (d.rotation === true) return { ...s, rotating: true };
  return { ...s, rotating: false, reloadSeq: s.reloadSeq + 1 };
}

function onError(state: TranscriptState, d: Obj): TranscriptState {
  const s = endTurn(state, state.waitingTasks);
  const message = text(d.error) || 'The turn stopped on an error.';
  // The chat is archived even when the turn that folds it fails.
  if (d.rotation === true) return { ...s, error: message, rotating: true };
  return { ...s, error: message, rotating: false, reloadSeq: s.reloadSeq + 1 };
}

/**
 * The chat on the page is a past chat now and a fresh one took its place. Everything painted goes, and the page is unloaded until the GET brings the fresh chat. Messages
 * still pending stay: they were held for the new chat, and the GET says what became of them.
 *
 * Only a page that knows of the rotation acts on it. One that has already settled on the fresh chat (a
 * GET got there first) hears the stream's fallback for a lost `rotation_done` as old news; clearing
 * again would take the fresh chat's live rows with it.
 */
function onRotationDone(state: TranscriptState): TranscriptState {
  if (!state.rotating) return state;
  return {
    ...state,
    loaded: false,
    rows: [],
    running: false,
    thinking: emptyThinking,
    rotating: false,
    generation: null,
    rotationSeq: state.rotationSeq + 1,
    reloadSeq: state.reloadSeq + 1,
    atBottom: true,
    unreadBelow: 0,
  };
}

function upsertPending(state: TranscriptState, p: PendingMessage): TranscriptState {
  // Already delivered: the transcript has it.
  if (state.rows.some((r) => r.kind === 'message' && r.role === 'sent' && r.ts === p.queued_at && !r.pending)) {
    return state;
  }
  const at = state.pending.findIndex((x) => x.id === p.id);
  if (at < 0) return { ...state, pending: [...state.pending, { ...p }] };
  return { ...state, pending: state.pending.map((x, i) => (i === at ? { ...x, ...p, deletable: x.deletable && p.deletable } : x)) };
}

function toPendingMessage(d: Obj): PendingMessage | null {
  const id = text(d.id);
  if (!id) return null;
  const atts = Array.isArray(d.attachments) ? d.attachments.map((a) => ({ sha: text(obj(a).sha), name: text(obj(a).name) })) : [];
  return { id, text: text(d.text), queued_at: num(d.queued_at) ?? 0, attachments: atts, deletable: d.deletable !== false };
}

function onPendingDelivered(state: TranscriptState, d: Obj): TranscriptState {
  const id = text(d.id);
  const ts = num(d.ts);
  const p = state.pending.find((x) => x.id === id);
  let s: TranscriptState = { ...state, pending: state.pending.filter((x) => x.id !== id) };
  if (ts === undefined || !p) return s;
  if (s.rows.some((r) => r.kind === 'message' && r.role === 'sent' && r.ts === ts)) return s;
  // The message lands where it was delivered: below the text the agent had written, after the pause marker.
  s = closeReply(s);
  return append(s, sentRow(ts, p.text, p.attachments.map(toAttachmentRef), true));
}

function onEvent(state: TranscriptState, name: string, raw: unknown, at: number): TranscriptState {
  const d = obj(raw);
  switch (name) {
    case 'delta':
      return onDelta(state, d, at);
    case 'thinking':
      return onThinking(state, d);
    case 'tool_use_start':
      return onToolUseStart(state, d, at);
    case 'tool_input_delta':
      return onToolInputDelta(state, d);
    case 'tool_use':
      return onToolUse(state, d);
    case 'tool_result':
      return onToolResult(state, d, at);
    case 'doc_published':
      return onDocPublished(state, d, at);
    case 'file_shared':
      return onFileShared(state, d, at);
    case 'subagent_started':
      return onSubagentStarted(state, d, at);
    case 'subagent_progress':
      return onSubagentProgress(state, d, at);
    case 'subagent_activity':
      return onSubagentActivity(state, d, at);
    case 'subagent_completed':
      return onSubagentCompleted(state, d, at);
    case 'chat_message':
      return onChatMessage(state, d);
    case 'chat_marker':
      return onChatMarker(state, d);
    case 'wake_update':
      return onWakeUpdate(state, d, at);
    case 'chat_fill':
      return onChatFill(state, d);
    case 'done':
      return onDone(state, d);
    case 'rotation_done':
      return onRotationDone(state);
    case 'error':
      return onError(state, d);
    case 'pending_message': {
      const p = toPendingMessage(d);
      return p ? upsertPending(state, p) : state;
    }
    case 'pending_offered': {
      const id = text(d.id);
      return { ...state, pending: state.pending.map((p) => (p.id === id ? { ...p, deletable: false } : p)) };
    }
    case 'pending_delivered':
      return onPendingDelivered(state, d);
    case 'pending_deleted': {
      const id = text(d.id);
      return { ...state, pending: state.pending.filter((p) => p.id !== id) };
    }
    default:
      return state;
  }
}

// ---- The reducer ----

export function reduceTranscript(state: TranscriptState, action: TranscriptAction): TranscriptState {
  switch (action.type) {
    case 'loaded':
      return loaded(state, action.chat, action.at);
    case 'event':
      return onEvent(state, action.name, action.data, action.at);
    case 'stream_open': {
      // The hub replays what it still holds: close the open reply so its deltas rebuild it, not double it.
      const s = closeReply(state);
      return s.thinking.since ? s : { ...s, thinking: { ...s.thinking, since: action.at } };
    }
    case 'stream_closed': {
      // A 204 (nothing running) or a dropped connection. Waiting on tasks outlives the stream.
      const s = closeReply(state);
      // A rotation this page was following went out of its sight, and `rotation_done` with it: the turn
      // was over before the stream opened (a 204), or the connection dropped. The GET says where it stands.
      if (s.waitingTasks > 0) return { ...s, running: false, reloadSeq: s.rotating ? s.reloadSeq + 1 : s.reloadSeq };
      // The turn's end was missed: a message was sent and never painted, or a connected stream ended
      // mid-turn without `done` (the hub was gone when the browser reconnected). The server has the rest.
      // A probe that never connected (the 204 at page load) is not that: refetching on it would churn.
      const lost = s.awaiting.length > 0 || (action.wasOpen === true && s.running) || s.rotating;
      return { ...s, running: false, thinking: emptyThinking, reloadSeq: lost ? s.reloadSeq + 1 : s.reloadSeq };
    }
    case 'thinking_promote':
      return promote(state);
    case 'send_posted': {
      const r = action.response;
      if (r.pending_id) {
        return upsertPending(state, {
          id: r.pending_id,
          text: action.text,
          queued_at: r.ts,
          attachments: r.attachments ?? [],
          deletable: true,
        });
      }
      // Delivered to an idle agent: painted when chat_message (or the next GET) brings it, never before.
      const painted = state.rows.some((row) => row.kind === 'message' && row.role === 'sent' && row.ts === r.ts);
      return {
        ...state,
        running: true,
        waitingTasks: 0,
        awaiting: painted || state.awaiting.includes(r.ts) ? state.awaiting : [...state.awaiting, r.ts],
        thinking: state.thinking.since ? state.thinking : { ...emptyThinking, since: action.at },
      };
    }
    case 'rotation_requested':
      // The turn that folds the chat has started; its stream brings the request's row and the rest.
      return {
        ...state,
        rotating: true,
        running: true,
        waitingTasks: 0,
        thinking: state.thinking.since ? state.thinking : { ...emptyThinking, since: action.at },
      };
    case 'pending_busy':
      return {
        ...state,
        pending: state.pending.map((p) => (p.id === action.id ? { ...p, sending: action.op === 'sending', deleting: action.op === 'deleting' } : p)),
      };
    case 'pending_removed':
      return { ...state, pending: state.pending.filter((p) => p.id !== action.id) };
    case 'pending_restored': {
      const m = action.message;
      if (m.delivered_ts) return { ...state, reloadSeq: state.reloadSeq + 1 };
      return upsertPending(state, { id: m.id, text: m.text, queued_at: m.queued_at, attachments: m.attachments, deletable: m.deletable });
    }
    case 'waiting': {
      const n = Math.max(0, action.tasks);
      if (n === state.waitingTasks) return state;
      if (n === 0) return { ...state, waitingTasks: 0, thinking: state.running ? state.thinking : emptyThinking };
      return { ...state, waitingTasks: n, thinking: { ...emptyThinking, since: state.thinking.since } };
    }
    case 'settings':
      return { ...state, settings: { ...state.settings, currentModel: action.currentModel, currentEffort: action.currentEffort } };
    case 'scrolled':
      return action.atBottom === state.atBottom && (!action.atBottom || state.unreadBelow === 0)
        ? state
        : { ...state, atBottom: action.atBottom, unreadBelow: action.atBottom ? 0 : state.unreadBelow };
    case 'dismiss_error':
      return state.error ? { ...state, error: null } : state;
  }
}

// ---- Selectors ----

/** True while an agent reply shows its caret: text is arriving right now. */
export function isStreaming(state: TranscriptState): boolean {
  return state.rows.some((r) => r.caret);
}

/** Stop is offered while a turn runs and while its background tasks do: Stop cancels both. */
export function canStop(state: TranscriptState): boolean {
  return state.running || state.waitingTasks > 0;
}

export interface ThinkingView {
  label: string;
  /** Start of the elapsed clock (unix ms); 0 hides it. */
  since: number;
}

function plural(n: number, one: string, many: string): string {
  return n + ' ' + (n === 1 ? one : many);
}

/**
 * The running dots at the bottom of the turn and their words, or null when none show. Never while text is
 * streaming: dots mean thinking or waiting, the caret means text is arriving.
 */
export function thinkingView(state: TranscriptState): ThinkingView | null {
  if (isStreaming(state)) return null;
  if (state.waitingTasks > 0 && !state.running) {
    return { label: 'Waiting on ' + plural(state.waitingTasks, 'task', 'tasks'), since: state.thinking.since };
  }
  if (!state.running) return null;
  const t = state.thinking;
  let label = 'Thinking';
  if (t.steps > 0) label += ' · ' + plural(t.steps, 'step', 'steps');
  if (t.summary) label += ' — ' + t.summary;
  return { label, since: t.since };
}

/** "m:ss" from ms. */
export function formatElapsed(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  return Math.floor(total / 60) + ':' + String(total % 60).padStart(2, '0');
}

/** The dots' full line: the label plus the elapsed clock. */
export function thinkingLine(view: ThinkingView, now: number): string {
  return view.since ? view.label + ' · ' + formatElapsed(now - view.since) : view.label;
}

// ---- Blocks: how rows group on the page ----

/**
 * What the page draws, in order. An agent's turn is one Message: its text, then the tool calls, background
 * tasks, published documents and shared files that followed, in the order they happened. Each text reply
 * starts a new block, so every model-written reply carries its own model badge.
 */
export type Block =
  | { type: 'agent'; key: string; from: ChatParty; ts: number; text?: ChatRow; items: ChatRow[] }
  | { type: 'row'; key: string; row: ChatRow };

export interface BlockOptions {
  /** Show the runtime's wake notes (hidden by default). */
  showWake?: boolean;
  /**
   * In a background task's transcript both parties are agents and every row is "received": rows from this
   * slug (the agent that dispatched the task) are the sent side, drawn on their own rather than as a turn.
   */
  senderSlug?: string;
}

/** True for a message on the sent side of the conversation: yours, or the dispatching agent's in a task transcript. */
export function isSentSide(r: ChatRow, senderSlug?: string): boolean {
  if (r.kind !== 'message') return false;
  if (senderSlug) return r.from?.slug === senderSlug;
  return r.role === 'sent' || r.from?.kind === 'person';
}

function isAgentText(r: ChatRow, senderSlug?: string): boolean {
  return r.kind === 'message' && !isSentSide(r, senderSlug);
}

function isAgentItem(r: ChatRow): boolean {
  return r.kind === 'tool_use' || r.kind === 'subagents' || r.kind === 'doc_published' || (r.kind === 'file_shared' && r.from?.kind !== 'person');
}

export function toBlocks(rows: readonly ChatRow[], agent: ChatParty, opts: BlockOptions = {}): Block[] {
  const out: Block[] = [];
  for (const r of rows) {
    if (r.kind === 'wake_update' && !opts.showWake) continue;
    if (isAgentText(r, opts.senderSlug)) {
      out.push({ type: 'agent', key: 'b:' + r.key, from: r.from ?? agent, ts: r.ts, text: r, items: [] });
      continue;
    }
    if (isAgentItem(r)) {
      const last = out[out.length - 1];
      if (last?.type === 'agent') last.items.push(r);
      else out.push({ type: 'agent', key: 'b:' + r.key, from: r.kind === 'file_shared' && r.from ? r.from : agent, ts: r.ts, items: [r] });
      continue;
    }
    out.push({ type: 'row', key: r.key, row: r });
  }
  return out;
}

// ---- Following the org snapshot ----

export interface ReconcileInput {
  /** This agent's state in the snapshot; null when the snapshot does not list it (archived, or another org). */
  agentState: string | null;
  /**
   * The snapshot's count of this agent's background tasks still running. The state alone cannot say: an
   * agent held after Stop is also `waiting`, with nothing running.
   */
  snapshotWaiting: number;
  /** A stream to this agent is open. */
  streamOpen: boolean;
  /** Armed when the tab came back into view; consumed by the next snapshot. */
  resumed: boolean;
  /** A turn was showing when the tab was hidden. */
  midTurnAtHide: boolean;
  /** The transcript's own view right now. */
  running: boolean;
  waitingTasks: number;
}

export interface ReconcileDecision {
  /** Open the stream; `force` reopens one that may look alive but died while the tab was hidden. */
  open?: 'if-closed' | 'force';
  /** Fetch the chat again: the turn ended while the tab was away and its events never arrived. */
  refetch?: boolean;
  /** The waiting count to apply (the snapshot sustains and retires waiting once `done` closed the stream). */
  waiting?: number;
}

/**
 * What a new org snapshot means for an open chat. Steady state only ever opens a missing stream (a turn
 * started from a delivery, or a stream that dropped); acting on "not working" there would race the turn's
 * own `done`. Right after the tab comes back, a working agent gets a fresh stream and a finished turn gets
 * a refetch.
 */
export function reconcileSnapshot(input: ReconcileInput): ReconcileDecision {
  if (input.agentState === null) return {};
  const working = input.agentState === 'running';
  const snapWaiting = Math.max(0, input.snapshotWaiting);
  const out: ReconcileDecision = {};
  // The count is the only channel still reporting on a batch once `done` closed the stream: it sustains the
  // waiting label, follows it down as tasks land, and retires it. A live
  // turn outranks it: the turn's own events label the dots.
  if (snapWaiting > 0) {
    if (!working) out.waiting = snapWaiting;
  } else if (input.waitingTasks > 0 && !input.running) {
    // The batch finished. Its task events went to a stream that `done` closed, so settle the rows from the server.
    out.waiting = 0;
    out.refetch = true;
  }
  if (input.resumed) {
    if (working) out.open = 'force';
    // Waiting on background work is a steady state, not a turn that ended while the tab was away.
    else if (snapWaiting === 0 && (input.midTurnAtHide || input.running)) out.refetch = true;
    return out;
  }
  if (working && !input.streamOpen) out.open = 'if-closed';
  return out;
}
