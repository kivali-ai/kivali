import * as React from 'react';
export type AssignmentReadiness = 'ready' | 'blocked' | 'held' | 'done' | 'dropped';
/**
 * Props for AssignmentRef.
 */
export interface AssignmentRefProps {
  id: number;
  title?: string;
  state?: AssignmentReadiness;
  href?: string }
export declare function AssignmentRef(props: AssignmentRefProps): JSX.Element;
