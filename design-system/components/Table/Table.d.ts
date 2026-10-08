import * as React from 'react';
/**
 * Props for Table.
 * @startingPoint section="Layout" subtitle="Data table with mono headers" viewport="700x260"
 */
export interface TableProps {
  className?: string;
  columns: { key: string;
  label: string;
  align?: 'left' | 'right' | 'center';
  width?: number | string;
  mono?: boolean;
  render?(row: any): React.ReactNode }[];
  rows: any[];
  rowKey?: string;
  dense?: boolean }
export declare function Table(props: TableProps): JSX.Element;
