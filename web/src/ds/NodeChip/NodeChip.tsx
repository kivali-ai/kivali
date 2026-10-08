import type { CSSProperties } from 'react';
import { cx } from '../cx';
import { Icon } from '../Icon/Icon';

export type NodeKind = 'decision' | 'requirement' | 'artifact';
export type NodeStatus = 'active' | 'superseded' | 'flagged' | 'unresolved';

export interface NodeChipProps {
  id: string;
  kind?: NodeKind;
  summary?: string;
  status?: NodeStatus;
  owner?: string;
  ownerName?: string;
  href?: string;
  selected?: boolean;
}

/**
 * One node of the knowledge graph: a decision, a requirement, or a plain artifact.
 *
 * - `kind` sets the glyph: `decision` is a filled diamond, `requirement` an outlined square, `artifact` a faint circle. `id` (mono) and a one-line `summary`.
 * - `owner` is the owning agent's identity color, shown as a small square so clusters read by color; pass `ownerName` for its tooltip.
 * - `status`: `active`; `superseded` (struck through and faded: still findable, no longer binding); `flagged` (a signal-soft tint with a warning icon: something needs attention); `unresolved` (a dashed edge with a danger link icon: it rests on something that does not exist).
 * - `selected` outlines it in cobalt. Kind badges on rows stay neutral.
 */
export function NodeChip({ id, kind = 'artifact', summary, status = 'active', owner, ownerName, href, selected = false }: NodeChipProps) {
  const cls = cx('kv-node', 'kv-node--' + kind, 'kv-node--' + status, selected && 'is-selected');
  const title = status !== 'active' ? id + ' · ' + status : id;
  const style = owner ? ({ '--node-owner': 'var(--identity-' + owner + ')' } as CSSProperties) : undefined;
  const body = (
    <>
      <span className="kv-node-glyph" aria-hidden="true" />
      <span className="kv-node-text">
        <span className="kv-node-id">{id}</span>
        {summary && <span className="kv-node-summary">{summary}</span>}
      </span>
      {owner && <span className="kv-node-owner" title={ownerName || owner} />}
      {status === 'flagged' && <Icon name="triangle-alert" size={14} label="Flagged" />}
      {status === 'unresolved' && <Icon name="link" size={14} label="Unresolved link" />}
    </>
  );
  return href ? (
    <a href={href} className={cls} title={title} style={style}>
      {body}
    </a>
  ) : (
    <span className={cls} title={title} style={style}>
      {body}
    </span>
  );
}
