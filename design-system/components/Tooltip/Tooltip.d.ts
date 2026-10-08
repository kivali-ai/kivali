import * as React from 'react';
/**
 * Props for Tooltip.
 */
export interface TooltipProps {
  content: React.ReactNode;
  side?: 'top' | 'right' | 'bottom' | 'left';
  open?: boolean;
  children: React.ReactElement }
export declare function Tooltip(props: TooltipProps): JSX.Element;
