import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
import { AgentState } from '../AgentState/AgentState.jsx';
import { SubagentTask } from '../SubagentTask/SubagentTask.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

export function SubagentGroup({ tasks = [], children, defaultOpen }) {
  const count = (st) => tasks.filter((t) => t.state === st).length;
  const running = count('running'), errored = count('errored'), done = count('done');
  const worst = errored ? 'errored' : running ? 'running' : 'done';
  const parts = [running && running + ' running', done && done + ' done', errored && errored + ' failed'].filter(Boolean).join(' · ');
  return (
    <details className={cx('kv-subgroup', 'kv-sub--' + worst)} open={!!defaultOpen}>
      <summary>
        <Icon name="users" size={14} />
        <span className="kv-sub-title">{tasks.length} subagent{tasks.length === 1 ? '' : 's'}</span>
        <span className="kv-tool-right"><span className="kv-tool-dur">{parts}</span><AgentState state={worst} compact /><Icon name="chevron-down" size={14} /></span>
      </summary>
      <div className="kv-subgroup-body">{children || tasks.map((t, i) => <SubagentTask key={t.id || i} {...t} />)}</div>
    </details>
  );
}
