import * as React from 'react';
/**
 * Props for Icon.
 */
export interface IconProps {
  className?: string;
  style?: React.CSSProperties;
  name: string;
  size?: 16 | 20 | 24 | number;
  strokeWidth?: number;
  label?: string }
export declare function Icon(props: IconProps): JSX.Element;
export declare const ICONS: Record<string, any[]>;
export declare const iconNames: string[];
