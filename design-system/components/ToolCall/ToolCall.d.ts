import * as React from 'react';
/**
 * Props for ToolCall.
 */
export interface ToolCallProps {
  name: string;
  status?: 'running' | 'done' | 'error';
  summary?: string;
  input?: unknown;
  output?: unknown;
  error?: string;
  duration?: string;
  defaultOpen?: boolean }
export declare function ToolCall(props: ToolCallProps): JSX.Element;
