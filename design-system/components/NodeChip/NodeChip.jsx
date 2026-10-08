import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

// The knowledge graph's unit: decisions are diamonds, requirements squares, artifacts faint circles.
export function NodeChip({ id, kind = 'artifact', summary, status = 'active', owner, ownerName, href, selected = false }) {
  const Tag = href ? 'a' : 'span';
  return (
    <Tag href={href} className={cx('kv-node', 'kv-node--' + kind, 'kv-node--' + status, selected && 'is-selected')}
      title={status !== 'active' ? id + ' · ' + status : id}
      style={owner ? { '--node-owner': 'var(--identity-' + owner + ')' } : undefined}>
      <span className="kv-node-glyph" aria-hidden="true" />
      <span className="kv-node-text">
        <span className="kv-node-id">{id}</span>
        {summary && <span className="kv-node-summary">{summary}</span>}
      </span>
      {owner && <span className="kv-node-owner" title={ownerName || owner} />}
      {status === 'flagged' && <Icon name="triangle-alert" size={14} label="Flagged" />}
      {status === 'unresolved' && <Icon name="link" size={14} label="Unresolved link" />}
    </Tag>
  );
}
