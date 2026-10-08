import * as React from 'react';
export type AgentRunState = 'running' | 'idle' | 'queued' | 'blocked' | 'held' | 'errored' | 'done' | 'cancelled';
/**
 * Props for AgentState.
 */
export interface AgentStateProps {
  className?: string;
  state: AgentRunState;
  label?: string;
  compact?: boolean;
}
export declare function AgentState(props: AgentStateProps): JSX.Element;
