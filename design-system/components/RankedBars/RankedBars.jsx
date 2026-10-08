import React from 'react';
import { Progress } from '../Progress/Progress.jsx';

// Who spent it: the top N as single bars, everyone else as a quiet total.
export function RankedBars({ items = [], top = 5, format = (v) => '$' + Math.round(v), restLabel = 'Everyone else' }) {
  const sorted = [...items].sort((a, b) => b.value - a.value);
  const head = sorted.slice(0, top), tail = sorted.slice(top);
  const max = Math.max(1, ...head.map((i) => i.value));
  const rest = tail.reduce((a, i) => a + i.value, 0);
  return (
    <div className="kv-ranked">
      {head.map((i) => <div key={i.label} className="kv-ranked-row"><span className="kv-ranked-label">{i.label}</span><Progress value={i.value} max={max} tone="cobalt" /><span className="kv-ranked-value">{format(i.value)}</span></div>)}
      {tail.length > 0 && <div className="kv-ranked-row kv-ranked-row--rest"><span className="kv-ranked-label">{restLabel} · {tail.length} {tail.length === 1 ? 'agent' : 'agents'}</span><span /><span className="kv-ranked-value">{format(rest)}</span></div>}
    </div>
  );
}
