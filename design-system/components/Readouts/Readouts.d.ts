import * as React from 'react';
/**
 * Props for Readouts.
 */
export interface ReadoutsProps {
  items: { n: React.ReactNode; label: string; href?: string }[];
}
export declare function Readouts(props: ReadoutsProps): JSX.Element;
