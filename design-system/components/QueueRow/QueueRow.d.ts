import * as React from 'react';
type Agent = { name: string; role?: string; color?: string; initials?: string };
/**
 * Props for QueueRow.
 */
export interface QueueRowProps {
  from: Agent;
  to: Agent[];
  kind?: 'notice' | 'assignment';
  refId?: number | string;
  title: React.ReactNode;
  releasesIn?: number;
  held?: boolean;
  selectable?: boolean;
  selected?: boolean;
  onSelect?(v: boolean | 'indeterminate'): void;
  onRelease?(): void;
  onToggle?(): void;
  expanded?: boolean;
  children?: React.ReactNode;
}
export declare function QueueRow(props: QueueRowProps): JSX.Element;
