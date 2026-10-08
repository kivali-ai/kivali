import React from 'react';
import { AgentAvatar } from '../AgentAvatar/AgentAvatar.jsx';
import { PersonAvatar } from '../PersonAvatar/PersonAvatar.jsx';
import { Badge } from '../Badge/Badge.jsx';
import { Button } from '../Button/Button.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

// Chat reads as a transcript: agents write on the page, people's words sit in a raised bubble.
// Additions: `model` (mono badge for the model and effort a reply used) and `pending` (a person's message not yet delivered).
export function Message({ from = {}, time, streaming = false, model, pending = false, onDelete, onSendNow, sendNowLoading = false, children, className }) {
  const kind = from.kind || 'agent';
  if (kind === 'system') {
    return <div className={cx('kv-msg', 'kv-msg--system', className)} role="note"><span className="kv-msg-rule" /><span>{children}</span>{time && <time>{time}</time>}<span className="kv-msg-rule" /></div>;
  }
  const avatar = kind === 'person'
    ? <PersonAvatar name={from.name} src={from.src} size={32} />
    : <AgentAvatar name={from.name} role={from.role} color={from.color} size={32} />;
  return (
    <div className={cx('kv-msg', 'kv-msg--' + kind, streaming && 'is-streaming', pending && 'is-pending', className)}>
      {avatar}
      <div className="kv-msg-main">
        <div className="kv-msg-head"><span className="kv-msg-name">{from.name}</span>{time && <time>{time}</time>}{pending && <Badge tone="cobalt">Pending</Badge>}</div>
        <div className="kv-msg-body">{children}{streaming && <span className="kv-caret" aria-hidden="true" />}</div>
        {model && kind === 'agent' && <div className="kv-msg-foot"><Badge variant="outline" mono>{model}</Badge></div>}
        {pending && (
          <div className="kv-msg-pending">
            <span>Waiting for a moment to deliver</span>
            {onDelete && <Button size="sm" variant="secondary" disabled={sendNowLoading} onClick={onDelete}>Delete</Button>}
            <Button size="sm" variant="primary" loading={sendNowLoading} disabled={sendNowLoading} onClick={onSendNow}>Send now</Button>
          </div>
        )}
      </div>
    </div>
  );
}
