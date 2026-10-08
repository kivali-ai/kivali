import * as React from 'react';
/**
 * Props for RankedBars.
 */
export interface RankedBarsProps {
  items: { label: string; value: number }[];
  top?: number;
  format?(v: number): string;
  restLabel?: string;
}
export declare function RankedBars(props: RankedBarsProps): JSX.Element;
