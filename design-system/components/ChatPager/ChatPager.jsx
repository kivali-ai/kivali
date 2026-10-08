import React from 'react';
import { Button } from '../Button/Button.jsx';
import { Icon } from '../Icon/Icon.jsx';

// Pages through one agent's chats like a book.
export function ChatPager({ title, index, total, onOlder, onNewer, onCurrent, compact = false }) {
  return (
    <div className="kv-pager">
      <Button variant="ghost" size="sm" iconOnly={compact} icon={<Icon name="chevron-left" />} aria-label="Older chat" disabled={index <= 1} onClick={onOlder}>Older</Button>
      <div className="kv-pager-mid"><span className="kv-pager-title">{title}</span><span className="kv-pager-sub">{index} of {total}</span></div>
      <Button variant="ghost" size="sm" iconOnly={compact} icon={<Icon name="chevron-right" />} aria-label="Newer chat" disabled={index >= total} onClick={onNewer}>Newer</Button>
      {onCurrent && <Button variant="secondary" size="sm" onClick={onCurrent}>Current chat</Button>}
    </div>
  );
}
