import type { ReactNode } from 'react';
import { cx } from '../cx';
import { AgentAvatar } from '../AgentAvatar/AgentAvatar';
import type { AgentRef } from '../AgentAvatar/AgentAvatar';
import { Badge } from '../Badge/Badge';
import { Button } from '../Button/Button';
import { Checkbox } from '../Checkbox/Checkbox';
import { Icon } from '../Icon/Icon';

export interface QueueRowProps {
  from: AgentRef;
  to: AgentRef[];
  kind?: 'notice' | 'assignment';
  refId?: number | string;
  title: ReactNode;
  releasesIn?: number;
  held?: boolean;
  selectable?: boolean;
  selected?: boolean;
  onSelect?(v: boolean | 'indeterminate'): void;
  onRelease?(): void;
  onToggle?(): void;
  expanded?: boolean;
  children?: ReactNode;
}

const fmt = (s: number) => Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');

/**
 * One message waiting in the queue between agents.
 *
 * - Release is one step, secondary. The Queue's one primary stays "Release all" (or "Release N selected").
 * - The countdown is mono and fixed when the message lands. It reads "Held" when auto-release is Off.
 * - `selectable` shows the checkbox; use it only when auto-release is Off.
 * - Expanded children hold the body, a note field ("Delivered as top-priority direction from you."), Raw message, Bounce (notices only, comment required) and "Release with note".
 */
export function QueueRow({ from, to = [], kind = 'notice', refId, title, releasesIn, held = false, selectable = false, selected = false, onSelect, onRelease, onToggle, expanded = false, children }: QueueRowProps) {
  const countdown = held ? 'Held' : releasesIn === 0 ? 'Releasing' : releasesIn != null ? 'Releases in ' + fmt(releasesIn) : null;
  const first = to[0];
  return (
    <div className={cx('kv-qrow', selected && 'is-selected')}>
      <div className="kv-qrow-line">
        {selectable && <Checkbox label="" ariaLabel="Select message" checked={selected} onCheckedChange={onSelect} />}
        <button type="button" className="kv-qrow-main" onClick={onToggle} aria-expanded={expanded}>
          <span className="kv-qrow-route">
            <AgentAvatar name={from.name} role={from.role} color={from.color} initials={from.initials} size={24} />
            <Icon name="arrow-right" size={14} />
            {first && <AgentAvatar name={first.name} role={first.role} color={first.color} initials={first.initials} size={24} />}
            {to.length > 1 && <span className="kv-qrow-plus">+{to.length - 1}</span>}
          </span>
          <span className="kv-qrow-text">
            <span className="kv-qrow-title">{title}</span>
            <span className="kv-qrow-meta">
              {from.name} → {to.map((t) => t.name).join(', ')}
              <Badge variant="outline" mono>
                {kind === 'assignment' ? 'assignment #' + refId : 'notice'}
              </Badge>
            </span>
          </span>
        </button>
        <span className="kv-qrow-trail">
          {countdown && <span className="kv-qrow-count">{countdown}</span>}
          <Button size="sm" variant="secondary" onClick={onRelease}>
            Release
          </Button>
        </span>
      </div>
      {expanded && <div className="kv-qrow-body">{children}</div>}
    </div>
  );
}
