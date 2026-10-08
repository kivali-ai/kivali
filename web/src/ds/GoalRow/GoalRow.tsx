import type { ReactNode } from 'react';
import { AgentAvatar } from '../AgentAvatar/AgentAvatar';
import type { AgentRef } from '../AgentAvatar/AgentAvatar';
import { AgentState } from '../AgentState/AgentState';
import type { AgentRunState } from '../AgentState/AgentState';
import { Progress } from '../Progress/Progress';

export type GoalWorker = AgentRef & { state?: AgentRunState };

export interface GoalRowProps {
  title: ReactNode;
  href?: string;
  owner?: AgentRef;
  done?: number;
  total?: number;
  blocker?: ReactNode;
  workers?: GoalWorker[];
  maxWorkers?: number;
}

/**
 * One goal (a top-level assignment) on Home: title, owner, progress from its parts, what is blocked and on whom, and who is working under it.
 *
 * - `maxWorkers` is 3 on desktop and 2 on phone; the rest collapse to "+N more" with their names in the tooltip.
 * - Say the blocker in words ("Waiting on #42, Run the tests", "On hold by you"). Never colour alone.
 * - Put rows in one raised container, hairline-divided. Not cards.
 */
export function GoalRow({ title, href, owner, done = 0, total = 0, blocker, workers = [], maxWorkers = 3 }: GoalRowProps) {
  const shown = workers.slice(0, maxWorkers);
  const rest = workers.slice(maxWorkers);
  return (
    <div className="kv-goal">
      <div className="kv-goal-top">
        {href ? (
          <a className="kv-goal-title" href={href}>
            {title}
          </a>
        ) : (
          <span className="kv-goal-title">{title}</span>
        )}
        <span className="kv-goal-progress">
          <Progress value={done} max={total || 1} aria-label={`${done} of ${total}`} />
          <span className="kv-goal-count">
            {done} of {total}
          </span>
        </span>
      </div>
      <div className="kv-goal-meta">
        {owner && (
          <span className="kv-goal-owner">
            <AgentAvatar name={owner.name} role={owner.role} color={owner.color} initials={owner.initials} size={24} />
            {owner.name}
          </span>
        )}
        {blocker && <span className="kv-goal-blocker">{blocker}</span>}
        <span className="kv-goal-workers">
          {shown.map((w) => (
            <span key={w.name} className="kv-goal-worker">
              <AgentAvatar name={w.name} role={w.role} color={w.color} initials={w.initials} size={20} />
              <AgentState state={w.state || 'running'} compact />
            </span>
          ))}
          {rest.length > 0 && (
            <span className="kv-goal-more" title={rest.map((w) => w.name).join(', ') + ' also working'}>
              +{rest.length} more
            </span>
          )}
        </span>
      </div>
    </div>
  );
}
