import type { CSSProperties, ReactNode } from 'react';
import { cx } from '../cx';
import { Icon } from '../Icon/Icon';

export interface CardProps {
  title?: ReactNode;
  meta?: ReactNode;
  actions?: ReactNode;
  collapsible?: boolean;
  defaultOpen?: boolean;
  tone?: 'attention';
  children?: ReactNode;
  className?: string;
  style?: CSSProperties;
}

/**
 * A raised container for one thing: an inbox item, a review, an agent, an assignment.
 *
 * - `title` (Space Grotesk), `meta` (a line of muted details), `actions` (usually `sm` buttons, the primary last), and the body as children.
 * - `collapsible` turns it into a disclosure: the header toggles the body; `defaultOpen` sets the first state.
 * - `tone="attention"` gives it a solid ink edge for the one card that needs the person now.
 * - Do not nest cards; use rows inside a card instead.
 * - Keep the two kinds of status apart. The card's own status (needs your approval, approved, failed) is a `Badge`. An agent's run state (`AgentState`) appears only beside that agent's name, and only when it matters to the decision.
 * - A card whose status needs the person shows it as a `Badge` in the meta line; a settled card says it in plain words with the time.
 */
export function Card({ title, meta, actions, collapsible = false, defaultOpen = true, tone, className, style, children }: CardProps) {
  const head = (title || meta || actions) && (
    <div className="kv-card-head">
      <div className="kv-card-titles">
        {title && <h3 className="kv-card-title">{title}</h3>}
        {meta && <div className="kv-card-meta">{meta}</div>}
      </div>
      {actions && <div className="kv-card-actions">{actions}</div>}
      {collapsible && (
        <span className="kv-card-chev">
          <Icon name="chevron-down" />
        </span>
      )}
    </div>
  );
  if (collapsible) {
    return (
      <details className={cx('kv-card', 'kv-card--collapsible', tone && 'kv-card--' + tone, className)} style={style} open={defaultOpen}>
        <summary>{head}</summary>
        <div className="kv-card-body">{children}</div>
      </details>
    );
  }
  return (
    <section className={cx('kv-card', tone && 'kv-card--' + tone, className)} style={style}>
      {head}
      {children && <div className="kv-card-body">{children}</div>}
    </section>
  );
}
