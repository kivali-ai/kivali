import * as React from 'react';
/**
 * Props for NodeChip.
 */
export interface NodeChipProps {
  id: string;
  kind?: 'decision' | 'requirement' | 'artifact';
  summary?: string;
  status?: 'active' | 'superseded' | 'flagged' | 'unresolved';
  owner?: string;
  ownerName?: string;
  href?: string;
  selected?: boolean }
export declare function NodeChip(props: NodeChipProps): JSX.Element;
