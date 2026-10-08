import * as React from 'react';
/**
 * Props for DayBars.
 */
export interface DayBarsProps {
  values: number[];
  width?: number;
  height?: number;
  max?: number;
  format?(v: number): string;
  labels?: [number, string][];
  color?: string;
  ariaLabel?: string;
}
export declare function DayBars(props: DayBarsProps): JSX.Element;
