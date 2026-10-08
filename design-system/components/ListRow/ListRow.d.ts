import * as React from 'react';
/**
 * Props for ListRow.
 */
export interface ListRowProps {
  /** For a row that opens content below it: sets aria-expanded on the row button. */
  expanded?: boolean;
  lead?: React.ReactNode;
  title: React.ReactNode;
  meta?: React.ReactNode;
  trail?: React.ReactNode;
  href?: string;
  onClick?(): void;
  selected?: boolean }
export declare function ListRow(props: ListRowProps): JSX.Element;
