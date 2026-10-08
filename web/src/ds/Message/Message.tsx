import type { ReactNode } from 'react';
import { cx } from '../cx';
import { AgentAvatar } from '../AgentAvatar/AgentAvatar';
import { Badge } from '../Badge/Badge';
import { Button } from '../Button/Button';
import { PersonAvatar } from '../PersonAvatar/PersonAvatar';

export type MessageFrom =
  | { kind: 'agent'; name: string; role?: string; color?: string }
  | { kind: 'person'; name: string; src?: string }
  | { kind: 'system' };

export interface MessageProps {
  from: MessageFrom;
  time?: string;
  streaming?: boolean;
  model?: string;
  pending?: boolean;
  /** Offered only while the message can still be taken back; without it the pending line has no Delete. */
  onDelete?(): void;
  onSendNow?(): void;
  /** Send now was clicked and the delivery has not landed yet: the button shows its loading dots. */
  sendNowLoading?: boolean;
  className?: string;
  children: ReactNode;
}

/**
 * One turn in a conversation: from an agent, from a person, or from the system.
 *
 * Chat reads as a transcript: agents write on the page, people's words sit in a raised bubble.
 *
 * - `from` is `{kind, name, ...}`. Agents (`kind: "agent"`) pass `role` and `color` for their `AgentAvatar` and write straight onto the page; people (`kind: "person"`) get a `PersonAvatar` and their words sit in a raised bubble. `kind: "system"` is a centered mono line between rules for events like rotations and handoffs.
 * - `time` is short ("6:02"); the full timestamp belongs in a tooltip. Children are the content, usually `Prose` for markdown, followed by any `Thinking`, `ToolCall` or `SubagentTask` blocks in the order they happened.
 * - `streaming` adds a blinking honey caret at the end while an agent is still writing.
 * - `model` shows the model and effort that wrote an agent reply as an outline mono badge at the foot ("opus · high"). Pass it on every model-written message. It shows for agent messages only.
 * - `pending` marks a person's message sent mid-turn and not yet delivered: a dashed edge, a cobalt "Pending" badge, and a line "Waiting for a moment to deliver" with Delete (secondary) and Send now (primary).
 *   Delete is drawn only when `onDelete` is given (the message can still be taken back); `sendNowLoading` puts Send now in its loading state until the delivery lands.
 * - After Send now, render a system message "Paused to deliver your message", then the messages as normal, where they were delivered.
 * - Running dots (Thinking, "Waiting on 2 tasks") and the streaming caret never appear on the same turn. Dots sit once at the bottom of the turn; the caret means text is arriving.
 */
export function Message({ from, time, streaming = false, model, pending = false, onDelete, onSendNow, sendNowLoading = false, children, className }: MessageProps) {
  if (from.kind === 'system') {
    return (
      <div className={cx('kv-msg', 'kv-msg--system', className)} role="note">
        <span className="kv-msg-rule" />
        <span>{children}</span>
        {time && <time>{time}</time>}
        <span className="kv-msg-rule" />
      </div>
    );
  }
  const kind = from.kind;
  const avatar =
    from.kind === 'person' ? (
      <PersonAvatar name={from.name} src={from.src} size={32} />
    ) : (
      <AgentAvatar name={from.name} role={from.role} color={from.color} size={32} />
    );
  return (
    <div className={cx('kv-msg', 'kv-msg--' + kind, streaming && 'is-streaming', pending && 'is-pending', className)}>
      {avatar}
      <div className="kv-msg-main">
        <div className="kv-msg-head">
          <span className="kv-msg-name">{from.name}</span>
          {time && <time>{time}</time>}
          {pending && <Badge tone="cobalt">Pending</Badge>}
        </div>
        <div className="kv-msg-body">
          {children}
          {streaming && <span className="kv-caret" aria-hidden="true" />}
        </div>
        {model && kind === 'agent' && (
          <div className="kv-msg-foot">
            <Badge variant="outline" mono>
              {model}
            </Badge>
          </div>
        )}
        {pending && (
          <div className="kv-msg-pending">
            <span>Waiting for a moment to deliver</span>
            {onDelete && (
              <Button size="sm" variant="secondary" disabled={sendNowLoading} onClick={onDelete}>
                Delete
              </Button>
            )}
            <Button size="sm" variant="primary" loading={sendNowLoading} disabled={sendNowLoading} onClick={onSendNow}>
              Send now
            </Button>
          </div>
        )}
      </div>
    </div>
  );
}
