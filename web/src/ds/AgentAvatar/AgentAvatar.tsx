import { createElement } from 'react';
import type { CSSProperties } from 'react';
import { cx } from '../cx';
import { ROLE_ICONS } from './roleIcons';

export { ROLE_ICONS };
export const roleIconNames: string[] = Object.keys(ROLE_ICONS);
export const identityColors: string[] = ['clay', 'ochre', 'olive', 'pine', 'lake', 'iris', 'plum', 'rose', 'slate'];

const SMALL_WORDS = new Set(['of', 'the', 'and', 'for', 'a', 'an', 'to', 'in', 'on', '&']);

export function initialsFor(name = ''): string {
  const words = name.split(/\s+/).filter((w) => w && !SMALL_WORDS.has(w.toLowerCase()));
  if (words.length === 1) return (words[0] ?? '').slice(0, 2).toUpperCase();
  return (((words[0] ?? '')[0] ?? '') + ((words[1] ?? '')[0] ?? '')).toUpperCase() || '?';
}

export function colorFor(name = ''): string {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0;
  return identityColors[h % identityColors.length] ?? 'slate';
}

function RoleGlyph({ name, size }: { name: string; size: number }) {
  const node = ROLE_ICONS[name] ?? [];
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.75}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      {node.map(([tag, attrs], i) => createElement(tag, { key: i, ...attrs }))}
    </svg>
  );
}

/** The fields that identify an agent to an avatar: what rows such as GoalRow and QueueRow take. */
export interface AgentRef {
  name: string;
  role?: string;
  color?: string;
  initials?: string;
}

export interface AgentAvatarProps {
  name: string;
  role?: string;
  color?: string;
  initials?: string;
  size?: 16 | 20 | 24 | 32 | 40 | 56 | number;
  className?: string;
}

/**
 * An agent's face: its initials top-left and its role icon bottom-right, on a rounded-square tile in one of the identity colors.
 *
 * - `name` is the agent's name; initials come from its first two meaningful words ("Chief of Staff" is CS), or the first two letters of a one-word name. Pass `initials` to override.
 * - `role` is a role icon name from `roleIconNames`: the one the agent's hiring agent picked. Without one (or with a name the set does not draw) the tile shows its initials only.
 * - `color` is one of `identityColors` (clay, ochre, olive, pine, lake, iris, plum, rose, slate). Without it, a color is derived from the name so it stays stable.
 * - `size`: 56 (profile header), 40 (cards, org chart), 32 (chat and lists, the smallest with the role icon), 24, 20, 16 (initials only, dense lists and the tree).
 * - Agents are always rounded squares and people are always circles (`PersonAvatar`). Never draw an agent as a circle or a person as a square, and never use a robot face for an agent.
 * - The app passes the identity color hashed from the agent's slug, so the same agent has the same color on every screen.
 */
export function AgentAvatar({ name, role, color, initials, size = 32, className }: AgentAvatarProps) {
  const c = color || colorFor(name);
  const ini = initials || initialsFor(name);
  const full = size >= 32 && !!role && role in ROLE_ICONS;
  const iniStyle: CSSProperties | undefined = full
    ? { left: Math.round(size * 0.14), top: Math.round(size * 0.12) }
    : undefined;
  return (
    <span
      className={cx('kv-avatar', 'kv-avatar--agent', full && 'kv-avatar--full', className)}
      role="img"
      aria-label={name}
      style={{
        width: size,
        height: size,
        borderRadius: Math.round(size * 0.28),
        background: 'var(--identity-' + c + ')',
        fontSize: Math.round(size * (full ? 0.3 : 0.4)),
      }}
    >
      <span className="kv-avatar-initials" style={iniStyle}>
        {ini}
      </span>
      {full && (
        <span className="kv-avatar-role" style={{ right: Math.round(size * 0.11), bottom: Math.round(size * 0.1) }}>
          <RoleGlyph name={role ?? ''} size={Math.round(size * 0.36)} />
        </span>
      )}
    </span>
  );
}
