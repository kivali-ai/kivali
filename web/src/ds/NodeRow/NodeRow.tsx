import { Fragment } from 'react';
import type { ReactNode } from 'react';
import { NodeChip } from '../NodeChip/NodeChip';
import type { NodeChipProps } from '../NodeChip/NodeChip';

export interface NodeRowProps {
  node: NodeChipProps;
  short?: ReactNode;
  expanded?: boolean;
  onToggle?(): void;
  fields?: [string, ReactNode][];
  compact?: boolean;
}

/**
 * A single-line graph node that opens to show its fields. One graph node per line, for lists of thousands.
 *
 * - `compact` (phone) drops the short fact so the chip never clips.
 * - Group rows under folded owner sections (Card collapsible, meta "41 nodes · 1 flagged"). Show a few, then "Show all N".
 * - Put search and the Flagged and Problems filters above the list.
 * - Uses NodeChip's own flagged, superseded and unresolved looks.
 */
export function NodeRow({ node, short, expanded = false, onToggle, fields = [], compact = false }: NodeRowProps) {
  return (
    <>
      <button type="button" className="kv-noderow" onClick={onToggle} aria-expanded={expanded}>
        <span className="kv-noderow-chip">
          <NodeChip {...node} />
        </span>
        {!compact && short && <span className="kv-noderow-short">{short}</span>}
      </button>
      {expanded && (
        <dl className="kv-noderow-fields">
          {fields.map(([k, v]) => (
            <Fragment key={k}>
              <dt>{k}</dt>
              <dd>{v}</dd>
            </Fragment>
          ))}
        </dl>
      )}
    </>
  );
}
