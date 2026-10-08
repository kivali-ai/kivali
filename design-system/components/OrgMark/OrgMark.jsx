import React from 'react';
const cx = (...c) => c.filter(Boolean).join(' ');

export function OrgMark({ name = '', src, color = 'slate', size = 28 }) {
  const ini = name.trim().split(/\s+/).map((w) => w[0]).join('').slice(0, 2).toUpperCase();
  return (
    <span className={cx('kv-orgmark', src && 'has-logo')} role="img" aria-label={name}
      style={{ width: size, height: size, borderRadius: Math.round(size * 0.24), background: src ? 'var(--paper-raised)' : 'var(--identity-' + color + ')', fontSize: Math.round(size * 0.42) }}>
      {src ? <img src={src} alt="" /> : ini}
    </span>
  );
}
