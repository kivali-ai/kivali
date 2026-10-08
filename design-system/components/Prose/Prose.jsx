import React from 'react';
const cx = (...c) => c.filter(Boolean).join(' ');

export function Prose({ children, html, className }) {
  return html != null ? <div className={cx('kv-prose', className)} dangerouslySetInnerHTML={{ __html: html }} />
    : <div className={cx('kv-prose', className)}>{children}</div>;
}
