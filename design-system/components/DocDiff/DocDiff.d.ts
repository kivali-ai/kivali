import * as React from 'react';
/**
 * Props for DocDiff.
 */
export interface DocDiffProps {
  before?: string | null;
  after: string;
  openChanged?: boolean;
}
export declare function DocDiff(props: DocDiffProps): JSX.Element;
export interface DiffLineProps {
  kind?: 'same' | 'add' | 'del';
  children?: React.ReactNode;
}
export declare function DiffLine(props: DiffLineProps): JSX.Element;
export declare function splitSections(md?: string): { title: string; lines: string[] }[];
