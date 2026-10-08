import React from 'react';

// A line of counts in label type, each a link to the view behind it.
export function Readouts({ items = [] }) {
  return (
    <div className="kv-readouts">
      {items.map((i, k) => (
        <React.Fragment key={i.label}>
          {k > 0 && <span className="kv-readouts-sep" aria-hidden="true">·</span>}
          {i.href ? <a className="kv-readout" href={i.href}><b>{i.n}</b> {i.label}</a> : <span className="kv-readout"><b>{i.n}</b> {i.label}</span>}
        </React.Fragment>
      ))}
    </div>
  );
}
