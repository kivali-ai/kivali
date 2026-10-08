import * as React from 'react';
/**
 * Props for ChatPager.
 */
export interface ChatPagerProps {
  title: React.ReactNode;
  index: number;
  total: number;
  onOlder?(): void;
  onNewer?(): void;
  onCurrent?(): void;
  compact?: boolean;
}
export declare function ChatPager(props: ChatPagerProps): JSX.Element;
