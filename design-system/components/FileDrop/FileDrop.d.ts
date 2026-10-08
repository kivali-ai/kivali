import * as React from 'react';
/**
 * Props for FileDrop.
 */
export interface FileDropProps {
  label?: string;
  hint?: string;
  accept?: string;
  multiple?: boolean;
  onFiles?(files: File[]): void }
export declare function FileDrop(props: FileDropProps): JSX.Element;
