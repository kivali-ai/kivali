import * as React from 'react';
/**
 * Props for OrgSwitcher.
 */
export interface OrgSwitcherProps {
  orgs: { id: string;
  name: string;
  src?: string;
  color?: string }[];
  current?: string;
  onSelect?(id: string): void;
  onCreate?(): void;
  defaultOpen?: boolean }
export declare function OrgSwitcher(props: OrgSwitcherProps): JSX.Element;
