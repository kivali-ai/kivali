import * as React from 'react';
/**
 * Props for TextField.
 */
export interface TextFieldProps extends React.InputHTMLAttributes<HTMLInputElement> {
  label: string;
  hint?: string;
  error?: string;
  multiline?: boolean;
  rows?: number;
}
export declare function TextField(props: TextFieldProps): JSX.Element;
