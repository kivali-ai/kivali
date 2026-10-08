import * as React from 'react';
/**
 * Props for ContextGauge.
 */
export interface ContextGaugeProps {
  value?: number;
  threshold?: number;
  onNewChat?(): void;
  compact?: boolean;
  title?: string;
}
export declare function ContextGauge(props: ContextGaugeProps): JSX.Element;
export interface ContextCountProps {
  value?: number;
  threshold?: number;
}
export declare function ContextCount(props: ContextCountProps): JSX.Element | null;
