import { memo, useMemo, useState } from 'react';
import { Link, useHref, useNavigate } from 'react-router';
import { apiGet } from '../../api/client';
import type { AttachmentRef, ChatParty, ToolCallDetail, TranscriptRow } from '../../api/types.gen';
import {
  Badge,
  Button,
  Card,
  Icon,
  AssignmentRef,
  Message,
  Prose,
  SubagentGroup,
  SubagentTask,
  Text,
  Thinking,
  ToolCall,
} from '../../ds';
import type { MessageFrom, SubagentGroupTask } from '../../ds';
import { renderMarkdown } from '../../lib/markdown';
import type { Identify } from '../../lib/agentIdentity';
import { useAgentIdentify, usePersonName } from '../../state/OrgProvider';
import { mergeLoaded, thinkingLine, toAttachmentRef, toBlocks } from '../../state/transcript';
import type { Block, ChatRow, LiveTask, PendingView, ThinkingView } from '../../state/transcript';
import { bareToolName } from '../../state/toolSummary';
import { duration, fileSize, messageTime, modelBadge } from './format';
import { asApiError } from './tabsShared';
import { SentRow } from './SentRow';

/** Server rows (a past chat, a background task) as transcript rows: keyed, nothing live. */
export function keyRows(rows: readonly TranscriptRow[]): ChatRow[] {
  return mergeLoaded(rows, []);
}

export interface TranscriptProps {
  rows: readonly ChatRow[];
  /** The agent whose chat this is: it writes the replies. */
  agent: { slug: string; name: string };
  /** Messages you sent mid-turn, drawn after everything else. */
  pending?: readonly PendingView[];
  /** The running dots at the bottom of the turn, or null. */
  thinking?: ThinkingView | null;
  /** Now, for message times and the elapsed clock. */
  now: number;
  /** An archived agent or a past chat: no pending messages or their actions. */
  readOnly?: boolean;
  /** A background task's transcript: the dispatching agent's slug, whose messages are the sent side. */
  senderSlug?: string;
  onDeletePending?(p: PendingView): void;
  onSendNow?(p: PendingView): void;
}

export const MarkdownBody = memo(function MarkdownBody({ md }: { md: string }) {
  const html = useMemo(() => renderMarkdown(md), [md]);
  return html ? <Prose html={html} className="app-chat-prose" /> : null;
});

function isMarkdownName(name: string): boolean {
  return /\.(md|markdown|mdown|mkd)$/i.test(name.trim());
}

function openAttachment(a: AttachmentRef): void {
  // Markdown opens rendered in a new tab; anything else downloads in place.
  if (isMarkdownName(a.name)) window.open(a.url, '_blank', 'noopener');
  else window.location.assign(a.url);
}

export function Attachments({ items }: { items: readonly AttachmentRef[] | undefined }) {
  if (!items || items.length === 0) return null;
  return (
    <div className="app-chat-attachments">
      {items.map((a, i) => {
        const size = fileSize(a.size_bytes);
        return (
          <Button key={a.url + i} variant="secondary" size="sm" icon={<Icon name="download" />} title={'Download ' + a.name} onClick={() => openAttachment(a)}>
            {size ? a.name + ' · ' + size : a.name}
          </Button>
        );
      })}
    </div>
  );
}

/** The sender of a row. You are drawn under your own name, as on the org chart, not the row's "You". */
/** Who a row is from: you, or an agent drawn as the sidebar draws it. A sender with no slug keeps its name only. */
function agentFrom(party: ChatParty | undefined, fallback: string, personName: string, identify: Identify): MessageFrom {
  if (party?.kind === 'person') return { kind: 'person', name: personName };
  if (!party?.slug) return { kind: 'agent', name: party?.name || fallback };
  return { kind: 'agent', ...identify(party) };
}

function prettyInput(input: string | undefined): string | undefined {
  if (!input) return undefined;
  try {
    return JSON.stringify(JSON.parse(input), null, 2);
  } catch {
    return input;
  }
}

/** Outputs are full text; the call is collapsed, and a very long payload is cut for the page. */
const OUTPUT_MAX = 20_000;

function clamp(s: string): string {
  return s.length > OUTPUT_MAX ? s.slice(0, OUTPUT_MAX) + '\n…' : s;
}

/** The current chat's API cuts long tool payloads; the full call is one request away. */
function toolCallUrl(slug: string, id: string): string {
  return '/api/v1/agents/' + encodeURIComponent(slug) + '/tool-calls/' + encodeURIComponent(id);
}

function ToolRow({ row, slug }: { row: ChatRow; slug: string }) {
  const [full, setFull] = useState<ToolCallDetail | null>(null);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  // A call listed as running with no turn in flight never got a result: say so instead of animating.
  const status = row.stale ? 'error' : (row.status ?? 'done');
  const failed = status === 'error';
  // Per part: a full copy fetched while the call ran has no output, so a cut output that lands later still offers it.
  const cut = !!row.tool_use_id && ((row.input_truncated === true && !full) || (row.output_truncated === true && !full?.output));
  const rawOutput = full?.output || row.output;
  const input = prettyInput(full ? full.input : row.input);
  const error = row.stale ? 'No result was recorded: the turn ended before this call came back.' : rawOutput || 'The call failed without saying why.';
  const took = duration(row.started_ts, row.ended_ts);
  const loadFull = async () => {
    setLoading(true);
    setLoadError(null);
    try {
      setFull(await apiGet<ToolCallDetail>(toolCallUrl(slug, row.tool_use_id ?? '')));
    } catch (err) {
      setLoadError(asApiError(err).message);
    } finally {
      setLoading(false);
    }
  };
  const more = cut ? (
    <div className="kv-tool-part">
      <Button variant="secondary" size="sm" disabled={loading} onClick={() => void loadFull()}>
        {loading ? 'Loading the full call…' : 'Show the full call'}
      </Button>
      {loadError && <span className="kv-tool-none">{loadError}</span>}
    </div>
  ) : undefined;
  return (
    <ToolCall
      name={bareToolName(row.name ?? 'tool')}
      status={status}
      {...(row.summary ? { summary: row.summary } : {})}
      {...(input !== undefined ? { input: clamp(input) } : {})}
      {...(failed ? { error: clamp(error) } : rawOutput ? { output: clamp(rawOutput) } : {})}
      {...(took ? { duration: took } : {})}
      {...(more ? { more } : {})}
    />
  );
}

/** A task as the task components draw it; `base` is where task sessions live. */
function groupTask(t: LiveTask, base: string): SubagentGroupTask {
  const task: SubagentGroupTask = { title: t.title || 'Background task', state: t.state };
  if (t.id) {
    task.id = t.id;
    task.sessionHref = base + encodeURIComponent(t.id);
  }
  // The same mono form as the reply badge ("sonnet 4.6 · low"); the component joins the two.
  if (t.model) task.model = modelBadge(t.model);
  if (t.effort) task.effort = t.effort.toLowerCase();
  if (t.state === 'running' && t.activity) task.latest = t.activity;
  if (t.state === 'errored') task.error = t.error || 'It stopped without saying why.';
  if (t.state === 'done' && t.output_md) task.result = <MarkdownBody md={t.output_md} />;
  return task;
}

function SubagentsRow({ row, slug }: { row: ChatRow; slug: string }) {
  const base = useHref('/agents/' + encodeURIComponent(slug) + '/subagents/');
  const tasks = (row.tasks ?? []).map((t) => groupTask(t, base));
  if (tasks.length === 0) return <SubagentGroup tasks={[]} />;
  if (tasks.length === 1 && tasks[0]) {
    const { id: _id, ...only } = tasks[0];
    return <SubagentTask {...only} />;
  }
  return <SubagentGroup tasks={tasks} />;
}

function DocRow({ row }: { row: ChatRow }) {
  return <SentRow row={row} />;
}

function AgentItem({ row, slug }: { row: ChatRow; slug: string }) {
  switch (row.kind) {
    case 'tool_use':
      return <ToolRow row={row} slug={slug} />;
    case 'subagents':
      return <SubagentsRow row={row} slug={slug} />;
    case 'doc_published':
      return <DocRow row={row} />;
    case 'file_shared':
      return (
        <div className="app-chat-shared">
          <MarkdownBody md={row.body_md ?? ''} />
          <Attachments items={row.attachments} />
        </div>
      );
    default:
      return null;
  }
}

function AgentBlock({ block, slug, agentName, now, thinking }: { block: Extract<Block, { type: 'agent' }>; slug: string; agentName: string; now: number; thinking: string | null }) {
  const personName = usePersonName();
  const identify = useAgentIdentify();
  const text = block.text;
  const badge = text ? modelBadge(text.model, text.effort) : '';
  return (
    <Message
      from={block.from.kind === 'agent' && block.from.slug === slug ? { kind: 'agent', ...identify({ slug, name: agentName }) } : agentFrom(block.from, agentName, personName, identify)}
      time={messageTime(block.ts, now)}
      streaming={!!text?.caret}
      {...(badge ? { model: badge } : {})}
    >
      {text &&
        (text.is_error ? (
          <Text as="p" tone="danger" className="app-chat-error-text">
            {text.body_md}
          </Text>
        ) : (
          <MarkdownBody md={text.body_md ?? ''} />
        ))}
      {text && <Attachments items={text.attachments} />}
      {block.items.length > 0 && (
        <div className="app-chat-items">
          {block.items.map((r) => (
            <AgentItem key={r.key} row={r} slug={slug} />
          ))}
        </div>
      )}
      {thinking && <Thinking active label={thinking} />}
    </Message>
  );
}

function DeliveryRow({ row, now }: { row: ChatRow; now: number }) {
  const assignmentBase = useHref('/assignments/');
  const to = row.delivered_to?.name;
  const personName = usePersonName();
  const identify = useAgentIdentify();
  const also = (row.also_to ?? []).map((p) => p.name || p.slug);
  const meta = [to ? 'Delivered to ' + to : '', also.length ? 'also to ' + also.join(', ') : ''].filter(Boolean).join(' · ');
  return (
    <Message from={agentFrom(row.from, 'Someone', personName, identify)} time={messageTime(row.ts, now)}>
      <Card title={row.title || 'Message'} {...(meta ? { meta } : {})} className="app-chat-card">
        <MarkdownBody md={row.body_md ?? ''} />
        <Attachments items={row.attachments} />
        <div className="app-chat-card-foot">
          {row.kind_badge && (
            <Badge variant="outline" mono>
              {row.kind_badge}
            </Badge>
          )}
          {row.assignment_ref && <AssignmentRef id={row.assignment_ref.id} {...(row.assignment_ref.title ? { title: row.assignment_ref.title } : {})} href={assignmentBase + row.assignment_ref.id} />}
          {row.replies_to && (
            <Text variant="caption" tone="muted">
              {'Replies go to ' + row.replies_to.name}
            </Text>
          )}
        </div>
      </Card>
    </Message>
  );
}

/**
 * Who a task's report is from. An avatar's colour follows its name unless one is given, and the name here
 * says the outcome: the neutral identity colour for all of them, since a task is no one on the team.
 */
const TASK_SENDER = 'Background task';
const TASK_COLOR = 'slate';

/**
 * A background task's report, where it reached the agent. The sender's line says how the task ended, and
 * the task sits below it, closed until asked for: what it answered, and the way to its full session. A
 * report that names no task holds its text the same way.
 */
function TaskResultRow({ row, now, slug }: { row: ChatRow; now: number; slug: string }) {
  const base = useHref('/agents/' + encodeURIComponent(slug) + '/subagents/');
  const reported = row.tasks?.[0];
  let name = TASK_SENDER;
  let body = (
    <Card title="Report" collapsible defaultOpen={false} className="app-chat-card">
      <MarkdownBody md={row.body_md ?? ''} />
    </Card>
  );
  if (reported) {
    name = TASK_SENDER + (reported.state === 'errored' ? ' failed' : ' finished');
    const { id: _id, ...task } = groupTask(reported, base);
    body = <SubagentTask {...task} />;
  }
  return (
    <Message from={{ kind: 'agent', name, color: TASK_COLOR }} time={messageTime(row.ts, now)}>
      {body}
    </Message>
  );
}

function CeoQueueRow({ row, now, readOnly }: { row: ChatRow; now: number; readOnly: boolean }) {
  const navigate = useNavigate();
  const personName = usePersonName();
  const identify = useAgentIdentify();
  const kind = row.message_type === 'ceo_approval_request' ? 'Asked for your approval' : 'Sent you a notice';
  return (
    <Message from={agentFrom(row.from, 'Someone', personName, identify)} time={messageTime(row.ts, now)}>
      <Card
        title={row.title || 'Message for you'}
        meta={row.resolved ? kind + ' · answered' : kind}
        className="app-chat-card"
        {...(!row.resolved && !readOnly
          ? {
              actions: (
                <Button size="sm" variant="secondary" onClick={() => void navigate('/')}>
                  Answer on Home
                </Button>
              ),
            }
          : {})}
      >
        <MarkdownBody md={row.body_md ?? ''} />
        <Attachments items={row.attachments} />
      </Card>
    </Message>
  );
}

function SingleRow({ row, now, slug, readOnly }: { row: ChatRow; now: number; slug: string; readOnly: boolean }) {
  const personName = usePersonName();
  const identify = useAgentIdentify();
  switch (row.kind) {
    case 'message':
      return (
        <Message from={row.from?.kind === 'agent' ? agentFrom(row.from, 'Someone', personName, identify) : { kind: 'person', name: personName }} time={messageTime(row.ts, now)}>
          <MarkdownBody md={row.body_md ?? ''} />
          <Attachments items={row.attachments} />
        </Message>
      );
    case 'delivery':
      return <DeliveryRow row={row} now={now} />;
    case 'ceo_queue':
      return <CeoQueueRow row={row} now={now} readOnly={readOnly} />;
    case 'task_result':
      return <TaskResultRow row={row} now={now} slug={slug} />;
    case 'marker':
      return (
        <Message from={{ kind: 'system' }} time={messageTime(row.ts, now)}>
          <span title={row.body_md || undefined}>{row.text || 'Turn boundary'}</span>
        </Message>
      );
    case 'rotation_prompt':
      return (
        <Message from={{ kind: 'system' }} time={messageTime(row.ts, now)}>
          New chat requested
        </Message>
      );
    case 'wake_update':
      return (
        <Card title="Wake note" meta={messageTime(row.ts, now)} collapsible defaultOpen={false} className="app-chat-card">
          <MarkdownBody md={row.body_md ?? ''} />
        </Card>
      );
    case 'file_shared':
      return (
        <Message from={{ kind: 'person', name: personName }} time={messageTime(row.ts, now)}>
          <MarkdownBody md={row.body_md ?? ''} />
          <Attachments items={row.attachments} />
        </Message>
      );
    default:
      return null;
  }
}

function PendingRow({ p, now, onDelete, onSendNow }: { p: PendingView; now: number; onDelete?(p: PendingView): void; onSendNow?(p: PendingView): void }) {
  const personName = usePersonName();
  // Delete is offered only while the message is still ours to take back: the
  // component draws it only when given a handler. Send now shows its loading dots until the delivery lands.
  const canDelete = p.deletable && !p.deleting && !!onDelete;
  return (
    <Message
      from={{ kind: 'person', name: personName }}
      time={messageTime(p.queued_at, now)}
      pending
      sendNowLoading={!!p.sending}
      {...(canDelete ? { onDelete: () => onDelete(p) } : {})}
      onSendNow={() => onSendNow?.(p)}
    >
      <MarkdownBody md={p.text} />
      <Attachments items={p.attachments.map(toAttachmentRef)} />
    </Message>
  );
}

/**
 * A transcript, oldest first: your messages, the agent's turns (text, tool calls, background tasks), the
 * deliveries it received, and the boundaries the runtime recorded. The chat, a past chat and a background
 * task's session all draw with it; `readOnly` drops the pending messages and their actions.
 */
export function Transcript({ rows, agent, pending = [], thinking = null, now, readOnly = false, senderSlug, onDeletePending, onSendNow }: TranscriptProps) {
  const [showWake, setShowWake] = useState(false);
  const identify = useAgentIdentify();
  const party = useMemo<ChatParty>(() => ({ kind: 'agent', slug: agent.slug, name: agent.name }), [agent.slug, agent.name]);
  const blocks = useMemo(() => toBlocks(rows, party, senderSlug ? { showWake, senderSlug } : { showWake }), [rows, party, showWake, senderSlug]);
  const wakeCount = useMemo(() => rows.filter((r) => r.kind === 'wake_update').length, [rows]);
  const line = thinking ? thinkingLine(thinking, now) : null;
  const last = blocks[blocks.length - 1];
  const dotsInLast = !!line && last?.type === 'agent';

  return (
    <div className="app-transcript">
      {wakeCount > 0 && (
        <div className="app-transcript-tools">
          <Button variant="ghost" size="sm" onClick={() => setShowWake((v) => !v)}>
            {showWake ? 'Hide wake notes' : 'Show wake notes (' + wakeCount + ')'}
          </Button>
        </div>
      )}
      <ol className="app-transcript-list" aria-label={'Chat with ' + agent.name}>
        {blocks.map((b, i) => (
          <li key={b.key} className="app-transcript-item">
            {b.type === 'agent' ? (
              <AgentBlock block={b} slug={agent.slug} agentName={agent.name} now={now} thinking={dotsInLast && i === blocks.length - 1 ? line : null} />
            ) : (
              <SingleRow row={b.row} now={now} slug={agent.slug} readOnly={readOnly} />
            )}
          </li>
        ))}
        {line && !dotsInLast && (
          <li className="app-transcript-item">
            <Message from={{ kind: 'agent', ...identify(agent) }}>
              <Thinking active label={line} />
            </Message>
          </li>
        )}
        {!readOnly &&
          pending.map((p) => (
            <li key={'p:' + p.id} className="app-transcript-item">
              <PendingRow p={p} now={now} {...(onDeletePending ? { onDelete: onDeletePending } : {})} {...(onSendNow ? { onSendNow } : {})} />
            </li>
          ))}
      </ol>
    </div>
  );
}
