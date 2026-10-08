import { useState } from 'react';
import { useHref } from 'react-router';
import { Badge, Card, Icon, AssignmentRef, ListRow } from '../../ds';
import type { ChatRow } from '../../state/transcript';
import { Attachments, MarkdownBody } from './Transcript';
import '../../styles/chat-sent.css';

/** "Engineering lead" or "a, b": the recipients as names. */
function names(row: ChatRow): string[] {
  return (row.to ?? []).map((p) => p.name || p.slug);
}

/** "Sent to Engineering lead · also to Buyer, tester": the first recipient, then the rest. */
function sentTo(to: readonly string[]): string {
  const [first, ...rest] = to;
  if (!first) return '';
  return 'Sent to ' + first + (rest.length ? ' · also to ' + rest.join(', ') : '');
}

/**
 * A message the agent published to someone other than you: one line naming it, which opens in place to the message as a
 * card, drawn the way a delivery is (title, recipients, body, attachments, the assignment, a raw link).
 *
 * - Collapsed by default, as every chat block starts. A message whose file could not be read has no body and does not open.
 * - The stored markdown file is only the quiet "Raw message" link inside the card; the line itself is never a link to it.
 */
export function SentRow({ row }: { row: ChatRow }) {
  const [open, setOpen] = useState(false);
  const assignmentBase = useHref('/assignments/');
  const to = names(row);
  const what = row.title ? '“' + row.title + '”' : 'a message';
  const line = 'Sent ' + what + (to.length ? ' to ' + to.join(', ') : '');
  const type = row.message_type ? row.message_type.replace(/_/g, ' ') : '';
  const hasBody = typeof row.body_md === 'string' && row.body_md.trim() !== '';
  const trail = (
    <>
      {type && (
        <Badge variant="outline" mono>
          {type}
        </Badge>
      )}
      {hasBody && <Icon name={open ? 'chevron-down' : 'chevron-right'} size={14} />}
    </>
  );
  const meta = sentTo(to);
  return (
    <div className="app-chat-sent">
      <ListRow
        className="app-chat-sent-line"
        lead={<Icon name="send" size={14} />}
        title={line}
        trail={trail}
        {...(hasBody ? { onClick: () => setOpen((o) => !o), expanded: open } : {})}
      />
      {hasBody && open && (
        <Card title={row.title || 'Message'} {...(meta ? { meta } : {})} className="app-chat-card">
          <MarkdownBody md={row.body_md ?? ''} />
          <Attachments items={row.attachments} />
          <div className="app-chat-card-foot">
            {type && (
              <Badge variant="outline" mono>
                {type}
              </Badge>
            )}
            {row.assignment_ref && (
              <AssignmentRef id={row.assignment_ref.id} {...(row.assignment_ref.title ? { title: row.assignment_ref.title } : {})} href={assignmentBase + row.assignment_ref.id} />
            )}
            {row.raw_url && (
              // eslint-disable-next-line react/forbid-elements -- external: the raw message is served outside the app
              <a className="app-chat-link app-chat-sent-raw" href={row.raw_url} target="_blank" rel="noopener noreferrer">
                Raw message
              </a>
            )}
          </div>
        </Card>
      )}
    </div>
  );
}
