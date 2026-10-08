import React from 'react';
const cx = (...c) => c.filter(Boolean).join(' ');

// Text in the Kivali type scale. Pick `as` for meaning (h1–h6 for headings) and `variant` for the look.
// Margins are zeroed; spacing belongs to the layout. Styles live in components/text.css.
export function Text({ as: Tag = 'span', variant = 'body', tone, className, children, ...rest }) {
  return <Tag className={cx('kv-text', 'kv-text-' + variant, tone && 'kv-text--' + tone, className)} {...rest}>{children}</Tag>;
}
