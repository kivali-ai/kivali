import * as React from 'react';
export type AssignmentReadiness = 'ready' | 'blocked' | 'held' | 'done' | 'dropped';
/**
 * Props for AssignmentState.
 */
export interface AssignmentStateProps {
  state: AssignmentReadiness;
  label?: string;
  compact?: boolean }
export declare function AssignmentState(props: AssignmentStateProps): JSX.Element;
export declare function AssignmentGlyph(p: { state: AssignmentReadiness; size?: number }): JSX.Element;
