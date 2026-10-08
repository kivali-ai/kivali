import React from 'react';
import { AgentAvatar } from '../AgentAvatar/AgentAvatar.jsx';
import { AgentState } from '../AgentState/AgentState.jsx';
import { Progress } from '../Progress/Progress.jsx';

// A top-level assignment and how it is going. Sits in one raised container with its siblings.
export function GoalRow({ title, href, owner, done = 0, total = 0, blocker, workers = [], maxWorkers = 3 }) {
  const shown = workers.slice(0, maxWorkers), rest = workers.slice(maxWorkers);
  return (
    <div className="kv-goal">
      <div className="kv-goal-top">
        {href ? <a className="kv-goal-title" href={href}>{title}</a> : <span className="kv-goal-title">{title}</span>}
        <span className="kv-goal-progress"><Progress value={done} max={total || 1} /><span className="kv-goal-count">{done} of {total}</span></span>
      </div>
      <div className="kv-goal-meta">
        {owner && <span className="kv-goal-owner"><AgentAvatar {...owner} size={24} />{owner.name}</span>}
        {blocker && <span className="kv-goal-blocker">{blocker}</span>}
        <span className="kv-goal-workers">
          {shown.map((w) => <span key={w.name} className="kv-goal-worker"><AgentAvatar {...w} size={20} /><AgentState state={w.state || 'running'} compact /></span>)}
          {rest.length > 0 && <span className="kv-goal-more" title={rest.map((w) => w.name).join(', ') + ' also working'}>+{rest.length} more</span>}
        </span>
      </div>
    </div>
  );
}
