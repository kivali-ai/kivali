import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

export function NavItem({ icon, label, count, attention = false, active = false, href, lead, depth = 0, onClick, className }) {
  const Tag = href ? 'a' : 'button';
  return (
    <Tag href={href} onClick={onClick} className={cx('kv-nav', active && 'is-active', className)} aria-current={active ? 'page' : undefined}
      style={depth ? { paddingLeft: 10 + depth * 16 } : undefined}>
      {lead || (icon && <Icon name={icon} size={18} />)}
      <span className="kv-nav-label">{label}</span>
      {count != null && count !== 0 && <span className={cx('kv-nav-count', attention && 'is-attention')}>{count}</span>}
    </Tag>
  );
}
