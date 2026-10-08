import * as React from 'react';
/**
 * Props for Checkbox.
 */
export interface CheckboxProps {
  id?: string;
  className?: string;
  name?: string;
  /** Accessible name when `label` is empty (a row-selection checkbox). */
  ariaLabel?: string;
  label: string;
  hint?: string;
  checked?: boolean | 'indeterminate';
  defaultChecked?: boolean;
  onCheckedChange?(v: boolean | 'indeterminate'): void;
  disabled?: boolean;
  name?: string;
}
export declare function Checkbox(props: CheckboxProps): JSX.Element;
