import React from 'react';
const cx = (...c) => c.filter(Boolean).join(' ');

export function ListRow({ lead, title, meta, trail, href, selected = false, expanded, onClick, className }) {
  const Tag = href ? 'a' : onClick ? 'button' : 'div';
  return (
    <Tag href={href} onClick={onClick} type={Tag === 'button' ? 'button' : undefined} aria-expanded={Tag === 'button' ? expanded : undefined} className={cx('kv-row', (href || onClick) && 'is-interactive', selected && 'is-selected', className)}>
      {lead && <span className="kv-row-lead">{lead}</span>}
      <span className="kv-row-main"><span className="kv-row-title">{title}</span>{meta && <span className="kv-row-meta">{meta}</span>}</span>
      {trail && <span className="kv-row-trail">{trail}</span>}
    </Tag>
  );
}
