import { cx } from '../cx';
import { initialsFor } from '../AgentAvatar/AgentAvatar';

export interface PersonAvatarProps {
  name: string;
  src?: string;
  size?: number;
  className?: string;
}

/**
 * A real person: a circle with their photo, or their initials on a neutral fill.
 *
 * - `name` gives the initials and the accessible name; `src` shows a photo instead.
 * - Always a circle, so a person is never confused with an agent (a rounded square). Sizes match `AgentAvatar`.
 * - People never get an identity color or a role icon; what they do is said in words beside the name.
 */
export function PersonAvatar({ name, src, size = 32, className }: PersonAvatarProps) {
  return (
    <span
      className={cx('kv-avatar', 'kv-avatar--person', className)}
      role="img"
      aria-label={name}
      style={{ width: size, height: size, fontSize: Math.round(size * 0.38) }}
    >
      {src ? <img src={src} alt="" /> : <span className="kv-avatar-initials">{initialsFor(name)}</span>}
    </span>
  );
}
