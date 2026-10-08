import React from 'react';
const cx = (...c) => c.filter(Boolean).join(' ');

const STATE_LABEL = { ready: 'Ready', blocked: 'Blocked', held: 'On hold', done: 'Done', dropped: 'Dropped' };
// Circles are work (agents are dots).
export function AssignmentGlyph({ state, size = 14 }) {
  const c = 7, r = 5.5;
  return (
    <svg className="kv-assignment-glyph" width={size} height={size} viewBox="0 0 14 14" aria-hidden="true">
      {state === 'done' ? <><circle cx={c} cy={c} r={6.5} className="g-fill" /><path d="M4.3 7.2l1.9 1.9 3.6-3.8" className="g-on" /></>
        : <circle cx={c} cy={c} r={r} className="g-ring" />}
      {state === 'ready' && <circle cx={c} cy={c} r={2} className="g-dot" />}
      {state === 'blocked' && <line x1={c} y1={3.6} x2={c} y2={10.4} className="g-line" />}
      {state === 'held' && <><line x1={5.6} y1={4.8} x2={5.6} y2={9.2} className="g-line" /><line x1={8.4} y1={4.8} x2={8.4} y2={9.2} className="g-line" /></>}
      {state === 'dropped' && <line x1={3.2} y1={10.8} x2={10.8} y2={3.2} className="g-line" />}
    </svg>
  );
}

export function AssignmentState({ state = 'ready', label, compact = false }) {
  const text = label || STATE_LABEL[state] || state;
  return (
    <span className={cx('kv-istate', 'kv-istate--' + state)} role="status" aria-label={compact ? text : undefined} title={compact ? text : undefined}>
      <AssignmentGlyph state={state} />{!compact && <span>{text}</span>}
    </span>
  );
}
