import type { ReactNode } from 'react';
import { cx } from '../cx';
import { Icon } from '../Icon/Icon';

export interface NavItemProps {
  icon?: string;
  lead?: ReactNode;
  /** Usually a string. The reference also passes a fragment (Home plus its compact running AgentState). */
  label: ReactNode;
  count?: ReactNode;
  attention?: boolean;
  active?: boolean;
  depth?: number;
  href?: string;
  onClick?(): void;
  className?: string;
}

/**
 * One entry in the sidebar: a place (Home, Team, Work, Graph, Org) or an agent in the org tree.
 *
 * - `icon` for places, or `lead` for anything else (an `AgentAvatar` at 20px for agents). `label` is required.
 * - `count` shows a quiet number; with `attention` it becomes a signal pill, reserved for things waiting on the person (the inbox). `count` can also hold a compact `AgentState`.
 * - `active` marks the current page with a raised tile; `depth` indents tree levels by 16px.
 * - `href` renders a link, otherwise a button with `onClick`.
 */
export function NavItem({ icon, label, count, attention = false, active = false, href, lead, depth = 0, onClick, className }: NavItemProps) {
  const style = depth ? { paddingLeft: 10 + depth * 16 } : undefined;
  const cls = cx('kv-nav', active && 'is-active', className);
  const aria = active ? ('page' as const) : undefined;
  const body = (
    <>
      {lead || (icon && <Icon name={icon} size={18} />)}
      <span className="kv-nav-label">{label}</span>
      {count != null && count !== 0 && <span className={cx('kv-nav-count', attention && 'is-attention')}>{count}</span>}
    </>
  );
  return href ? (
    <a href={href} onClick={onClick} className={cls} aria-current={aria} style={style}>
      {body}
    </a>
  ) : (
    <button type="button" onClick={onClick} className={cls} aria-current={aria} style={style}>
      {body}
    </button>
  );
}
