import * as React from 'react';
type Agent = { name: string; role?: string; color?: string; initials?: string };
type Worker = Agent & { state?: 'running' | 'idle' | 'queued' | 'blocked' | 'held' | 'errored' | 'done' | 'cancelled' };
/**
 * Props for GoalRow.
 */
export interface GoalRowProps {
  title: React.ReactNode;
  href?: string;
  owner?: Agent;
  done?: number;
  total?: number;
  blocker?: React.ReactNode;
  workers?: Worker[];
  maxWorkers?: number;
}
export declare function GoalRow(props: GoalRowProps): JSX.Element;
