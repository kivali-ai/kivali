import * as React from 'react';
/**
 * Props for Prose.
 */
export interface ProseProps {
  html?: string;
  children?: React.ReactNode;
  className?: string;
  style?: React.CSSProperties;
}
export declare function Prose(props: ProseProps): JSX.Element;
