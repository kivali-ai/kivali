import { cx } from '../cx';

export interface OrgMarkProps {
  name: string;
  src?: string;
  color?: string;
  size?: number;
}

/**
 * An org's mark: its uploaded logo, or, until one is uploaded, its initials on its identity color.
 *
 * - `name`, `src` (the uploaded logo) and `color` (the org's identity color). Sizes 20 (menus), 28 (the sidebar and phone header) and 40 (settings, headers).
 * - Orgs are rounded squares with a slightly tighter corner than agents, and always carry their own name next to them in lists.
 * - The org's brand lives here and in the org header only. It never recolors Kivali's chrome, buttons or status colors.
 * - The logo is a square mark shown whole inside the tile. Wide wordmarks do not fit; ask for the square symbol instead.
 * - It appears in the sidebar head and phone header, the org's settings and profile header, the browser tab and its sign-in page; not in Kivali's top bar, on buttons, or inside chats.
 */
export function OrgMark({ name = '', src, color = 'slate', size = 28 }: OrgMarkProps) {
  const ini = name
    .trim()
    .split(/\s+/)
    .map((w) => w[0] ?? '')
    .join('')
    .slice(0, 2)
    .toUpperCase();
  return (
    <span
      className={cx('kv-orgmark', src && 'has-logo')}
      role="img"
      aria-label={name}
      style={{
        width: size,
        height: size,
        borderRadius: Math.round(size * 0.24),
        background: src ? 'var(--paper-raised)' : 'var(--identity-' + color + ')',
        fontSize: Math.round(size * 0.42),
      }}
    >
      {src ? <img src={src} alt="" /> : ini}
    </span>
  );
}
