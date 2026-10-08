import React from 'react';
import { initialsFor } from '../AgentAvatar/AgentAvatar.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

// People are round. The shape alone says whether it's a person.
export function PersonAvatar({ name, src, size = 32, className }) {
  return (
    <span className={cx('kv-avatar', 'kv-avatar--person', className)} role="img" aria-label={name}
      style={{ width: size, height: size, fontSize: Math.round(size * 0.38) }}>
      {src ? <img src={src} alt="" /> : <span className="kv-avatar-initials">{initialsFor(name)}</span>}
    </span>
  );
}
