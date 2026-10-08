import type { ReactNode } from 'react';
import { cx } from '../cx';

export interface ListRowProps {
  lead?: ReactNode;
  title: ReactNode;
  meta?: ReactNode;
  trail?: ReactNode;
  href?: string;
  onClick?(): void;
  selected?: boolean;
  /** For a row that opens content below it: sets aria-expanded on the row's button. Absent, no attribute. */
  expanded?: boolean;
  className?: string;
}

/**
 * One row in a list: agents, files, chats, skills. The default way to show many things; cards are for the few that need a decision.
 *
 * - `lead` (avatar or icon), `title`, `meta` (one muted line that can hold `AgentState` or `Badge`), `trail` (time, a count, a small action).
 * - With `href` or `onClick` the whole row is the target; `selected` tints it cobalt-soft.
 * - Rows are separated by line hairlines. Put a list of rows inside one raised container rather than giving each row its own card.
 */
export function ListRow({ lead, title, meta, trail, href, selected = false, expanded, onClick, className }: ListRowProps) {
  const cls = cx('kv-row', (href || onClick) && 'is-interactive', selected && 'is-selected', className);
  const body = (
    <>
      {lead && <span className="kv-row-lead">{lead}</span>}
      <span className="kv-row-main">
        <span className="kv-row-title">{title}</span>
        {meta && <span className="kv-row-meta">{meta}</span>}
      </span>
      {trail && <span className="kv-row-trail">{trail}</span>}
    </>
  );
  if (href)
    return (
      <a href={href} onClick={onClick} className={cls}>
        {body}
      </a>
    );
  if (onClick)
    return (
      <button type="button" onClick={onClick} className={cls} aria-expanded={expanded}>
        {body}
      </button>
    );
  return <div className={cls}>{body}</div>;
}
