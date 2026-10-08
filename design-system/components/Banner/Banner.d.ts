import * as React from 'react';
/**
 * Props for Banner.
 */
export interface BannerProps {
  className?: string;
  tone?: 'info' | 'warning' | 'danger' | 'success';
  title?: React.ReactNode;
  action?: React.ReactNode;
  onDismiss?(): void;
  children?: React.ReactNode;
}
export declare function Banner(props: BannerProps): JSX.Element;
