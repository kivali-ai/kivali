import React from 'react';
const cx = (...c) => c.filter(Boolean).join(' ');

export function Badge({ tone = 'neutral', variant = 'soft', mono = false, className, children, ...rest }) {
  return <span className={cx('kv-badge', 'kv-badge--' + tone, 'kv-badge--' + variant, mono && 'kv-badge--mono', className)} {...rest}>{children}</span>;
}
