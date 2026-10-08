import type { ReactNode } from 'react';
import { cx } from '../cx';
import { AgentState } from '../AgentState/AgentState';
import { Icon } from '../Icon/Icon';

export interface ThinkingProps {
  active?: boolean;
  label?: string;
  seconds?: number;
  defaultOpen?: boolean;
  children?: ReactNode;
}

/**
 * An agent's reasoning between turns, collapsed by default, like every block in chat.
 *
 * - `active` shows the running dots with "Thinking"; once done, it becomes "Thought for 12s" (`seconds`) with a quiet brain icon. The person can open it to read the reasoning (children), set in italic behind a thin rule.
 * - Never show reasoning open by default in chat; it is there for people who want to check.
 */
export function Thinking({ active = true, label, children, defaultOpen = false, seconds }: ThinkingProps) {
  const text = label || (active ? 'Thinking' : 'Thought' + (seconds ? ' for ' + seconds + 's' : ''));
  return (
    <details className={cx('kv-think', active && 'is-active')} open={defaultOpen}>
      <summary>
        {active ? <AgentState state="running" compact label={text} /> : <Icon name="brain" size={14} />}
        <span>{text}</span>
        {children && <Icon name="chevron-down" size={14} />}
      </summary>
      {children && <div className="kv-think-body">{children}</div>}
    </details>
  );
}
