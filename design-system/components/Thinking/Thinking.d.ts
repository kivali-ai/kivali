import * as React from 'react';
/**
 * Props for Thinking.
 */
export interface ThinkingProps {
  active?: boolean;
  label?: string;
  seconds?: number;
  defaultOpen?: boolean;
  children?: React.ReactNode }
export declare function Thinking(props: ThinkingProps): JSX.Element;
