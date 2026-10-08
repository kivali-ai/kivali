import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
import { AgentState } from '../AgentState/AgentState.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

export function Thinking({ active = true, label, children, defaultOpen = false, seconds }) {
  const text = label || (active ? 'Thinking' : 'Thought' + (seconds ? ' for ' + seconds + 's' : ''));
  return (
    <details className={cx('kv-think', active && 'is-active')} open={defaultOpen}>
      <summary>
        {active ? <AgentState state="running" compact label={text} /> : <Icon name="brain" size={14} />}
        <span>{text}</span>{children && <Icon name="chevron-down" size={14} />}
      </summary>
      {children && <div className="kv-think-body">{children}</div>}
    </details>
  );
}
