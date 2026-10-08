import * as React from 'react';
/**
 * Props for Dialog.
 */
export interface DialogProps {
  open?: boolean;
  defaultOpen?: boolean;
  onOpenChange?(open: boolean): void;
  trigger?: React.ReactElement;
  title: React.ReactNode;
  description?: React.ReactNode;
  footer?: React.ReactNode;
  tone?: 'danger';
  children?: React.ReactNode;
}
export declare function Dialog(props: DialogProps): JSX.Element;
export declare function DialogClose(p: { asChild?: boolean; children: React.ReactElement }): JSX.Element;
