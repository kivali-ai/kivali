import * as React from 'react';
export type AutoReleaseStop = 'Now' | '30s' | '2m' | '5m' | '20m' | 'Off' | string;
/**
 * Props for AutoRelease.
 */
export interface AutoReleaseProps {
  value?: AutoReleaseStop;
  onChange?(value: AutoReleaseStop): void;
  stops?: AutoReleaseStop[];
  variant?: 'slider' | 'select';
  label?: string;
  className?: string;
}
export declare function AutoRelease(props: AutoReleaseProps): JSX.Element;
export declare const AUTO_RELEASE_STOPS: string[];
