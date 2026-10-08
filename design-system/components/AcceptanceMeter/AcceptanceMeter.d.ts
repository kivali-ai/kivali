import * as React from 'react';
/**
 * Props for AcceptanceMeter.
 */
export interface AcceptanceMeterProps {
  satisfied: number;
  claimed: number;
  unclaimed: number;
  compact?: boolean }
export declare function AcceptanceMeter(props: AcceptanceMeterProps): JSX.Element | null;
