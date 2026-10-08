import * as React from 'react';
export type AgentRunState = 'running' | 'idle' | 'queued' | 'blocked' | 'held' | 'errored' | 'done' | 'cancelled';
/**
 * Props for SubagentTask.
 */
export interface SubagentTaskProps {
  title: string;
  state?: AgentRunState;
  model?: string;
  effort?: string;
  elapsed?: string;
  latest?: string;
  result?: React.ReactNode;
  error?: string;
  sessionHref?: string;
  defaultOpen?: boolean;
  children?: React.ReactNode }
export declare function SubagentTask(props: SubagentTaskProps): JSX.Element;
