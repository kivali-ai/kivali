import { cx } from '../cx';
import { AssignmentGlyph } from '../AssignmentState/AssignmentState';
import type { AssignmentReadiness } from '../AssignmentState/AssignmentState';

export interface AssignmentRefProps {
  id: number;
  title?: string;
  state?: AssignmentReadiness;
  href?: string;
}

/**
 * A reference to an assignment inside text or a row: `#42`, optionally with its state and title.
 *
 * - `id` is required; `state` adds the `AssignmentState` circle so a reader sees at a glance whether the referenced work is done; `title` adds the name (truncated; the full title shows on hover); `href` makes it a link.
 * - Use it wherever assignments mention each other: "waiting on", parents, acceptance items, messages from agents.
 */
export function AssignmentRef({ id, title, state, href }: AssignmentRefProps) {
  const cls = cx('kv-iref', state && 'kv-iref--' + state);
  const tip = title ? '#' + id + ' ' + title : undefined;
  const body = (
    <>
      {state && <AssignmentGlyph state={state} size={12} />}
      <span className="kv-iref-id">#{id}</span>
      {title && <span className="kv-iref-title">{title}</span>}
    </>
  );
  return href ? (
    <a href={href} className={cls} title={tip}>
      {body}
    </a>
  ) : (
    <span className={cls} title={tip}>
      {body}
    </span>
  );
}
