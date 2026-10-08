import { cx } from '../cx';
import { AcceptanceMeter } from '../AcceptanceMeter/AcceptanceMeter';
import { AgentAvatar } from '../AgentAvatar/AgentAvatar';
import { Icon } from '../Icon/Icon';
import { AssignmentRef } from '../AssignmentRef/AssignmentRef';
import { AssignmentState } from '../AssignmentState/AssignmentState';
import type { AssignmentReadiness } from '../AssignmentState/AssignmentState';

export type AssignmentAssignee = { kind?: 'agent'; name: string; role?: string; color?: string } | { kind: 'person'; name: string };

export interface AssignmentRowProps {
  id: number;
  title: string;
  state?: AssignmentReadiness;
  assignee?: AssignmentAssignee;
  depth?: number;
  last?: boolean;
  openChildren?: number;
  acceptance?: { satisfied: number; claimed: number; unclaimed: number };
  waitingOn?: { id: number; state?: AssignmentReadiness; title?: string }[];
  heldBy?: string;
  expanded?: boolean;
  onToggle?(): void;
  href?: string;
  onClick?(): void;
  selected?: boolean;
}

/**
 * One assignment in a list or tree: the unit of the Work view.
 *
 * - `id`, `title`, `state` (the `AssignmentState` circle), `assignee` (an agent's `{name, role, color}` for its avatar, or `{kind: "person", name}` for a person), `openChildren` (open child count) and `acceptance` (`{satisfied, claimed, unclaimed}` for a compact `AcceptanceMeter`).
 * - Trees: `depth` indents 20px per level with elbow lines; `last` ends the line at the last child; `onToggle` and `expanded` add the disclosure chevron.
 * - Why it cannot move is said in the row: `waitingOn` lists the assignments it waits on as `AssignmentRef`s, and `heldBy` says who paused it, both in signal-ink. Done and dropped rows go quiet (dropped is struck through).
 *   An entry's optional `title` puts the reason in words after its ref ("Waiting on #45, Run the tests"); entries with titles are separated by semicolons. (Kivali web addition; the reference takes `{id, state}` only.)
 * - `onClick` or `href` makes the row a target; `selected` tints it cobalt-soft for the assignment shown in a detail pane.
 */
export function AssignmentRow({ id, title, state = 'ready', assignee, depth = 0, last = false, acceptance, openChildren: kids, waitingOn = [], heldBy, expanded, onToggle, href, onClick, selected = false }: AssignmentRowProps) {
  const cls = cx('kv-irow', 'kv-irow--' + state, (href || onClick) && 'is-interactive', selected && 'is-selected');
  const titled = waitingOn.some((w) => w.title);
  const body = (
    <>
      <span className="kv-irow-indent" style={{ width: depth * 20 }}>
        {depth > 0 && <span className={cx('kv-irow-elbow', last && 'is-last')} />}
      </span>
      {onToggle ? (
        <span
          role="button"
          tabIndex={0}
          className={cx('kv-irow-toggle', expanded && 'is-open')}
          aria-label={expanded ? 'Collapse' : 'Expand'}
          aria-expanded={!!expanded}
          onClick={(e) => {
            e.preventDefault();
            e.stopPropagation();
            onToggle();
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              e.stopPropagation();
              onToggle();
            }
          }}
        >
          <Icon name="chevron-right" size={14} />
        </span>
      ) : (
        <span className="kv-irow-toggle is-empty" />
      )}
      <AssignmentState state={state} compact />
      <span className="kv-irow-main">
        <span className="kv-irow-line">
          <span className="kv-irow-title">{title}</span>
          <span className="kv-irow-id">#{id}</span>
        </span>
        {(waitingOn.length > 0 || heldBy) && (
          <span className="kv-irow-why">
            {heldBy ? (
              <>On hold by {heldBy}</>
            ) : (
              <>
                Waiting on{' '}
                {waitingOn.map((w, i) => (
                  // One inline run per entry so the flex gap of .kv-irow-why never lands before a comma.
                  <span key={w.id}>
                    <AssignmentRef id={w.id} state={w.state} />
                    {w.title ? ', ' + w.title : ''}
                    {i < waitingOn.length - 1 ? (titled ? '; ' : ', ') : ''}
                  </span>
                ))}
              </>
            )}
          </span>
        )}
      </span>
      <span className="kv-irow-trail">
        {kids ? (
          <span className="kv-irow-kids" title="Open children">
            <Icon name="list-todo" size={14} />
            {kids}
          </span>
        ) : null}
        {acceptance && <AcceptanceMeter {...acceptance} compact />}
        {assignee &&
          (assignee.kind === 'person' ? (
            <span className="kv-irow-person" title={assignee.name}>
              {assignee.name
                .split(' ')
                .map((w) => w[0] ?? '')
                .join('')
                .slice(0, 2)}
            </span>
          ) : (
            <AgentAvatar name={assignee.name} role={assignee.role} color={assignee.color} size={20} />
          ))}
      </span>
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
      <button type="button" onClick={onClick} className={cls}>
        {body}
      </button>
    );
  return <div className={cls}>{body}</div>;
}
