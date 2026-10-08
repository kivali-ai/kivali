import type { AgentRunState } from '../ds';
import type { AgentState } from '../api/types.gen';

export interface RunStateView {
  state: AgentRunState;
  /** Overrides the word AgentState shows (its tooltip and accessible name when compact). */
  label?: string;
}

/**
 * The server's agent states mapped to the design system's run-state vocabulary. Idle has no view: the tree
 * shows nothing for an agent at rest.
 */
export function runStateView(state: AgentState, waitingTasks = 0): RunStateView | null {
  switch (state) {
    case 'running':
      return { state: 'running' };
    case 'waiting':
      // The snapshot says `waiting` both for background tasks still running and for an agent held after Stop
      // (org_state.go agentState); only the task count tells them apart.
      return { state: 'queued', label: waitingTasks > 0 ? 'Waiting on tasks' : 'Stopped by you' };
    case 'needs_help':
      return { state: 'errored' };
    case 'quarantined':
      return { state: 'errored', label: 'Stopped after repeated failures' };
    case 'disconnected':
      return { state: 'blocked', label: 'Not connected' };
    case 'idle':
      return null;
  }
}
