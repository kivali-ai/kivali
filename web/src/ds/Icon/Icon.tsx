import { createElement } from 'react';
import type { CSSProperties } from 'react';
import { cx } from '../cx';
import { ICONS } from './icons';

export { ICONS };
export const iconNames: string[] = Object.keys(ICONS);

export interface IconProps {
  name: string;
  size?: 16 | 20 | 24 | number;
  strokeWidth?: number;
  label?: string;
  className?: string;
  style?: CSSProperties;
}

/**
 * An outline icon from the Kivali set: a curated subset of Lucide (ISC), drawn at a 1.5px stroke in `currentColor`.
 *
 * - `name` is a kebab-case Lucide name from the set; `size` is 16 (inline with text, default), 20 (buttons and nav) or 24 (empty states and headers). Keep the 1.5px stroke.
 * - Decorative by default (hidden from screen readers). Pass `label` when the icon is the only thing carrying meaning, such as an icon-only button.
 * - The icon takes the text color around it; do not color icons on their own except for status (`danger`, `success`, `signal-ink`), and then always beside a word.
 * - Need an icon that is not here? Add it from Lucide to `icons.ts` rather than drawing one; keep the set small.
 */
export function Icon({ name, size = 16, strokeWidth = 1.5, label, className, style }: IconProps) {
  const node = ICONS[name];
  if (!node) return null;
  return (
    <svg
      className={cx('kv-icon', className)}
      style={style}
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={strokeWidth}
      strokeLinecap="round"
      strokeLinejoin="round"
      role={label ? 'img' : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
    >
      {node.map(([tag, attrs], i) => createElement(tag, { key: i, ...attrs }))}
    </svg>
  );
}
