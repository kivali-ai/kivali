import type { ReactNode } from 'react';
import { cx } from '../cx';

export interface EmptyStateProps {
  title: string;
  action?: ReactNode;
  children?: ReactNode;
  className?: string;
}

const HONEY: ReadonlyArray<readonly [number, number]> = [
  [2, 1],
  [5, 2],
];

/**
 * What a page says when it has nothing to show yet: an invitation, not an apology.
 *
 * - `title` names the space ("No agents yet", "Inbox zero"); children give one sentence of what goes here; `action` is one button, usually `primary`.
 * - The art is the brand texture: a dot grid with the logo's stepped pair set into it as two honey dots, three columns apart and one row down (the icon's proportions). Do not swap in illustrations.
 * - A little warmth is welcome here ("Every team starts with one."); keep it to one line.
 */
export function EmptyState({ title, children, action, className }: EmptyStateProps) {
  // The logo's stepped pair set into the brand dot grid.
  const P = 12;
  const cols = 8;
  const rows = 5;
  const r = 6.5;
  const dots = [];
  for (let y = 0; y < rows; y++) {
    for (let x = 0; x < cols; x++) {
      if (HONEY.some(([hx, hy]) => hx === x && hy === y)) continue;
      dots.push(<circle key={x + '-' + y} cx={6 + x * P} cy={6 + y * P} r="1.75" className="kv-empty-dot" />);
    }
  }
  return (
    <div className={cx('kv-empty', className)}>
      <svg className="kv-empty-art" width="120" height="75" viewBox="0 0 96 60" aria-hidden="true">
        {dots}
        {HONEY.map(([hx, hy]) => (
          <circle key={'h' + hx} cx={6 + hx * P} cy={6 + hy * P} r={r} className="kv-empty-honey" />
        ))}
      </svg>
      <h3 className="kv-empty-title">{title}</h3>
      {children && <p className="kv-empty-body">{children}</p>}
      {action && <div className="kv-empty-action">{action}</div>}
    </div>
  );
}
