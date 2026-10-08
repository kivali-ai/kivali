import React from 'react';
import { NodeChip } from '../NodeChip/NodeChip.jsx';

// One graph node per line, for lists of thousands.
export function NodeRow({ node, short, expanded = false, onToggle, fields = [], compact = false }) {
  return (
    <>
      <button type="button" className="kv-noderow" onClick={onToggle} aria-expanded={expanded}>
        <span className="kv-noderow-chip"><NodeChip {...node} /></span>
        {!compact && short && <span className="kv-noderow-short">{short}</span>}
      </button>
      {expanded && <dl className="kv-noderow-fields">{fields.map(([k, v]) => <React.Fragment key={k}><dt>{k}</dt><dd>{v}</dd></React.Fragment>)}</dl>}
    </>
  );
}
