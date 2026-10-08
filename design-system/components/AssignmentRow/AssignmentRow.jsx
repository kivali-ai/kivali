import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
import { AgentAvatar } from '../AgentAvatar/AgentAvatar.jsx';
import { AssignmentState } from '../AssignmentState/AssignmentState.jsx';
import { AssignmentRef } from '../AssignmentRef/AssignmentRef.jsx';
import { AcceptanceMeter } from '../AcceptanceMeter/AcceptanceMeter.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

export function AssignmentRow({ id, title, state = 'ready', assignee, depth = 0, last = false, acceptance, openChildren: kids, waitingOn = [], heldBy, expanded, onToggle, href, onClick, selected = false }) {
  const Tag = href ? 'a' : onClick ? 'button' : 'div';
  return (
    <Tag href={href} onClick={onClick} className={cx('kv-irow', 'kv-irow--' + state, (href || onClick) && 'is-interactive', selected && 'is-selected')}>
      <span className="kv-irow-indent" style={{ width: depth * 20 }}>
        {depth > 0 && <span className={cx('kv-irow-elbow', last && 'is-last')} />}
      </span>
      {onToggle ? <span role="button" tabIndex={0} className={cx('kv-irow-toggle', expanded && 'is-open')} aria-label={expanded ? 'Collapse' : 'Expand'} aria-expanded={!!expanded}
        onClick={(e) => { e.preventDefault(); e.stopPropagation(); onToggle(); }}
        onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); e.stopPropagation(); onToggle(); } }}><Icon name="chevron-right" size={14} /></span>
        : <span className="kv-irow-toggle is-empty" />}
      <AssignmentState state={state} compact />
      <span className="kv-irow-main">
        <span className="kv-irow-line"><span className="kv-irow-title">{title}</span><span className="kv-irow-id">#{id}</span></span>
        {(waitingOn.length > 0 || heldBy) && (
          <span className="kv-irow-why">
            {heldBy ? <>On hold by {heldBy}</> : <>Waiting on {waitingOn.map((w, i) => <span key={w.id}><AssignmentRef id={w.id} state={w.state} />{w.title ? ', ' + w.title : ''}{i < waitingOn.length - 1 ? (waitingOn.some((x) => x.title) ? '; ' : ', ') : ''}</span>)}</>}
          </span>
        )}
      </span>
      <span className="kv-irow-trail">
        {kids ? <span className="kv-irow-kids" title="Open children"><Icon name="list-todo" size={14} />{kids}</span> : null}
        {acceptance && <AcceptanceMeter {...acceptance} compact />}
        {assignee && (assignee.kind === 'person'
          ? <span className="kv-irow-person" title={assignee.name}>{assignee.name.split(' ').map((w) => w[0]).join('').slice(0, 2)}</span>
          : <AgentAvatar name={assignee.name} role={assignee.role} color={assignee.color} size={20} />)}
      </span>
    </Tag>
  );
}
