import * as React from 'react';
type From = { kind: 'agent'; name: string; role?: string; color?: string } | { kind: 'person'; name: string; src?: string } | { kind: 'system' };
/**
 * Props for Message.
 */
export interface MessageProps {
  from: From;
  time?: string;
  streaming?: boolean;
  model?: string;
  pending?: boolean;
  onDelete?(): void;
  onSendNow?(): void;
  /** Send now was clicked and delivery has not landed: the button shows loading dots. */
  sendNowLoading?: boolean;
  className?: string;
  children: React.ReactNode }
export declare function Message(props: MessageProps): JSX.Element;
