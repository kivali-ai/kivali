import * as React from 'react';
/**
 * Props for Switch.
 */
export interface SwitchProps {
  id?: string;
  className?: string;
  name?: string;
  label: string;
  hint?: string;
  checked?: boolean;
  defaultChecked?: boolean;
  onCheckedChange?(v: boolean): void;
  disabled?: boolean;
  name?: string;
}
export declare function Switch(props: SwitchProps): JSX.Element;
