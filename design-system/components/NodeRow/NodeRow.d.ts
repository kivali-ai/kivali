import * as React from 'react';
/**
 * Props for NodeRow.
 */
export interface NodeRowProps {
  node: { id: string; kind?: 'decision' | 'requirement' | 'artifact'; summary?: string; status?: 'active' | 'superseded' | 'flagged' | 'unresolved'; owner?: string; ownerName?: string; href?: string; selected?: boolean };
  short?: React.ReactNode;
  expanded?: boolean;
  onToggle?(): void;
  fields?: [string, React.ReactNode][];
  compact?: boolean;
}
export declare function NodeRow(props: NodeRowProps): JSX.Element;
