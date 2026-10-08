import type { ReactNode } from 'react';
import { cx } from '../cx';
import { AgentState } from '../AgentState/AgentState';
import type { AgentRunState } from '../AgentState/AgentState';
import { Icon } from '../Icon/Icon';
import { SubagentTask } from '../SubagentTask/SubagentTask';
import type { SubagentTaskProps } from '../SubagentTask/SubagentTask';

export type SubagentGroupTask = SubagentTaskProps & { id?: string };

export interface SubagentGroupProps {
  tasks?: SubagentGroupTask[];
  defaultOpen?: boolean;
  children?: ReactNode;
}

/**
 * Several subagents an agent started at once, gathered under one line.
 *
 * - The header counts them and sums up where they stand ("1 running · 2 done", with "failed" when any failed); its state glyph is the worst of the group.
 * - It starts collapsed; the header's counts and state say enough. Inside, each task is a `SubagentTask`, also collapsed.
 * - Pass `tasks` (each the props of a `SubagentTask`), or `SubagentTask` children for full control.
 */
export function SubagentGroup({ tasks = [], children, defaultOpen }: SubagentGroupProps) {
  const count = (st: AgentRunState) => tasks.filter((t) => t.state === st).length;
  const running = count('running');
  const errored = count('errored');
  const done = count('done');
  const worst: AgentRunState = errored ? 'errored' : running ? 'running' : 'done';
  const parts = [running && running + ' running', done && done + ' done', errored && errored + ' failed'].filter(Boolean).join(' · ');
  return (
    <details className={cx('kv-subgroup', 'kv-sub--' + worst)} open={!!defaultOpen}>
      <summary>
        <Icon name="users" size={14} />
        <span className="kv-sub-title">
          {tasks.length} subagent{tasks.length === 1 ? '' : 's'}
        </span>
        <span className="kv-tool-right">
          <span className="kv-tool-dur">{parts}</span>
          <AgentState state={worst} compact />
          <Icon name="chevron-down" size={14} />
        </span>
      </summary>
      <div className="kv-subgroup-body">{children || tasks.map((t, i) => <SubagentTask key={t.id || i} {...t} />)}</div>
    </details>
  );
}
