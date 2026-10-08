import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

const STATE_LABEL = { running: 'Working', idle: 'Idle', queued: 'Queued', blocked: 'Blocked', held: 'On hold', errored: 'Needs help', done: 'Done', cancelled: 'Cancelled' };
// Three dots, the logo's honey dots at work. Never a bare colored light.
export function AgentState({ state = 'idle', label, compact = false, className }) {
  const text = label || STATE_LABEL[state] || state;
  return (
    <span className={cx('kv-state', 'kv-state--' + state, compact && 'kv-state--compact', className)}
      role="status" aria-label={compact ? text : undefined} title={compact ? text : undefined}>
      <span className="kv-state-glyph" aria-hidden="true">
        {state === 'errored' ? <Icon name="triangle-alert" size={14} /> :
          state === 'done' ? <Icon name="check" size={14} /> :
            state === 'cancelled' ? <Icon name="x" size={14} /> :
              state === 'blocked' ? <><i /><i /><b className="kv-state-wall" /></> :
                state === 'held' ? <><b className="kv-state-bar" /><b className="kv-state-bar" /></> :
                  state === 'idle' ? <i /> :
                    <><i /><i /><i /></>}
      </span>
      {!compact && <span className="kv-state-text">{text}</span>}
    </span>
  );
}
