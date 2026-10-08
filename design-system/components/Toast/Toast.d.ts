import * as React from 'react';
/**
 * Props for Toast.
 */
export interface ToastProps {
  tone?: 'info' | 'success' | 'danger';
  title: React.ReactNode;
  action?: React.ReactNode;
  onDismiss?(): void;
  children?: React.ReactNode }
export declare function Toast(props: ToastProps): JSX.Element;
