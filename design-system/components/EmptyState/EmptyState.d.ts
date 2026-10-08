import * as React from 'react';
/**
 * Props for EmptyState.
 */
export interface EmptyStateProps {
  className?: string;
  title: string;
  action?: React.ReactNode;
  children?: React.ReactNode;
}
export declare function EmptyState(props: EmptyStateProps): JSX.Element;
