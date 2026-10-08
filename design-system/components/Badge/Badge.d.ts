import * as React from 'react';
type Tone = 'neutral' | 'signal' | 'cobalt' | 'success' | 'danger';
/**
 * Props for Badge.
 */
export interface BadgeProps {
  tone?: Tone;
  variant?: 'soft' | 'solid' | 'outline';
  mono?: boolean;
  children: React.ReactNode;
}
export declare function Badge(props: BadgeProps): JSX.Element;
