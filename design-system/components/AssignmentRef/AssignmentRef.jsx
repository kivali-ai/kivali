import React from 'react';
import { AssignmentGlyph } from '../AssignmentState/AssignmentState.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

export function AssignmentRef({ id, title, state, href }) {
  const Tag = href ? 'a' : 'span';
  return (
    <Tag href={href} className={cx('kv-iref', state && 'kv-iref--' + state)} title={title ? '#' + id + ' ' + title : undefined}>
      {state && <AssignmentGlyph state={state} size={12} />}<span className="kv-iref-id">#{id}</span>{title && <span className="kv-iref-title">{title}</span>}
    </Tag>
  );
}
