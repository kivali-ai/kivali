import * as React from 'react';
/**
 * Props for Select.
 */
export interface SelectProps extends React.SelectHTMLAttributes<HTMLSelectElement> {
  label: string;
  hint?: string;
  error?: string;
  options: { value: string;
  label: string }[];
}
export declare function Select(props: SelectProps): JSX.Element;
