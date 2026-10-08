import * as React from 'react';
export type AssignmentReadiness = 'ready' | 'blocked' | 'held' | 'done' | 'dropped';
/**
 * Props for AssignmentRow.
 * @startingPoint section="Work" subtitle="Assignment tree rows" viewport="700x330"
 */
export interface AssignmentRowProps {
  id: number;
  title: string;
  state: AssignmentReadiness;
  assignee?: { kind?: 'agent';
  name: string;
  role?: string;
  color?: string } | { kind: 'person';
  name: string };
  depth?: number;
  last?: boolean;
  openChildren?: number;
  acceptance?: { satisfied: number;
  claimed: number;
  unclaimed: number };
  waitingOn?: { id: number;
  state?: AssignmentReadiness;
  /** Shown after the ref; when any entry has one, entries are separated by semicolons. */
  title?: string }[];
  heldBy?: string;
  expanded?: boolean;
  onToggle?(): void;
  href?: string;
  onClick?(): void;
  selected?: boolean }
export declare function AssignmentRow(props: AssignmentRowProps): JSX.Element;
