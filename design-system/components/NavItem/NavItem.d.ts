import * as React from 'react';
/**
 * Props for NavItem.
 */
export interface NavItemProps {
  className?: string;
  icon?: string;
  lead?: React.ReactNode;
  label: string;
  count?: React.ReactNode;
  attention?: boolean;
  active?: boolean;
  depth?: number;
  href?: string;
  onClick?(): void }
export declare function NavItem(props: NavItemProps): JSX.Element;
