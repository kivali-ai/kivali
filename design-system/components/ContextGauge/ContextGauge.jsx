import React from 'react';
import { Progress } from '../Progress/Progress.jsx';
import { Badge } from '../Badge/Badge.jsx';
import { Button } from '../Button/Button.jsx';
import { Icon } from '../Icon/Icon.jsx';

const toneFor = (pct) => (pct < 50 ? 'success' : 'cobalt');

// How full an agent's chat is, beside the action that fixes it.
export function ContextGauge({ value = 0, threshold = 80, onNewChat, compact = false, title }) {
  const pct = Math.round(value);
  const long = pct >= threshold;
  return (
    <span className="kv-ctx" title={title}>
      {long
        ? <Badge tone="signal" variant="solid">Context {pct}% full</Badge>
        : <span className="kv-ctx-gauge"><span className="kv-ctx-label">Context <b>{pct}%</b></span><span className="kv-ctx-bar"><Progress value={pct} tone={toneFor(pct)} /></span></span>}
      {onNewChat && <Button variant={long ? 'primary' : 'secondary'} size={compact ? 'sm' : 'md'} icon={<Icon name="rotate-ccw" />} onClick={onNewChat}>New chat</Button>}
    </span>
  );
}

// For NavItem's count slot in the org tree and Team rows: only shown past the threshold.
export function ContextCount({ value = 0, threshold = 80 }) {
  const pct = Math.round(value);
  if (pct < threshold) return null;
  return <span title={'Context ' + pct + '% full · start a new chat'}><Badge tone="signal" mono>{pct}%</Badge></span>;
}
