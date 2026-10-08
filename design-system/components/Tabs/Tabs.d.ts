import * as React from 'react';
/**
 * Props for Tabs.
 */
export interface TabsProps {
  className?: string;
  tabs: { value: string;
  label: string;
  count?: number;
  content?: React.ReactNode }[];
  value?: string;
  defaultValue?: string;
  onValueChange?(v: string): void }
export declare function Tabs(props: TabsProps): JSX.Element;
