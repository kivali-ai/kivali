import * as React from 'react';
/**
 * Props for Progress.
 */
export interface ProgressProps {
  value: number;
  max?: number;
  label?: string;
  tone?: 'ink' | 'cobalt' | 'success' }
export declare function Progress(props: ProgressProps): JSX.Element;
