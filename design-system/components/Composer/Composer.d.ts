import * as React from 'react';
/**
 * Props for Composer.
 * @startingPoint section="Chat" subtitle="Message composer" viewport="700x190"
 */
export interface ComposerProps {
  /** The message may go out with no text (e.g. staged attachments). */
  allowEmpty?: boolean;
  placeholder?: string;
  value?: string;
  defaultValue?: string;
  onChange?(v: string): void;
  onSend?(v: string): void;
  busy?: boolean;
  onAttach?(): void;
  footer?: React.ReactNode;
  disabled?: boolean }
export declare function Composer(props: ComposerProps): JSX.Element;
