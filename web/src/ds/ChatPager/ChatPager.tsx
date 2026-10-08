import type { ReactNode } from 'react';
import { Button } from '../Button/Button';
import { Icon } from '../Icon/Icon';

export interface ChatPagerProps {
  title: ReactNode;
  index: number;
  total: number;
  onOlder?(): void;
  onNewer?(): void;
  onCurrent?(): void;
  compact?: boolean;
}

/**
 * A slim strip under an agent's tabs for stepping through its past chats, like a book.
 *
 * - `compact` makes the arrows icon-only for phone.
 * - Newer is disabled on the last chat, Older on the first. Leave out `onCurrent` for an archived agent.
 * - It sits under the tabs with a hairline, never as a banner.
 */
export function ChatPager({ title, index, total, onOlder, onNewer, onCurrent, compact = false }: ChatPagerProps) {
  return (
    <div className="kv-pager">
      <Button variant="ghost" size="sm" iconOnly={compact} icon={<Icon name="chevron-left" />} aria-label="Older chat" disabled={index <= 1} onClick={onOlder}>
        Older
      </Button>
      <div className="kv-pager-mid">
        <span className="kv-pager-title">{title}</span>
        <span className="kv-pager-sub">
          {index} of {total}
        </span>
      </div>
      <Button variant="ghost" size="sm" iconOnly={compact} icon={<Icon name="chevron-right" />} aria-label="Newer chat" disabled={index >= total} onClick={onNewer}>
        Newer
      </Button>
      {onCurrent && (
        <Button variant="secondary" size="sm" onClick={onCurrent}>
          Current chat
        </Button>
      )}
    </div>
  );
}
