import React from 'react';
import { AgentAvatar } from '../AgentAvatar/AgentAvatar.jsx';
import { Badge } from '../Badge/Badge.jsx';
import { Button } from '../Button/Button.jsx';
import { Checkbox } from '../Checkbox/Checkbox.jsx';
import { Icon } from '../Icon/Icon.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');
const fmt = (s) => Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');

// One delivery waiting between agents, with a one-step Release.
export function QueueRow({ from, to = [], kind = 'notice', refId, title, releasesIn, held = false, selectable = false, selected = false, onSelect, onRelease, onToggle, expanded = false, children }) {
  const countdown = held ? 'Held' : releasesIn === 0 ? 'Releasing' : releasesIn != null ? 'Releases in ' + fmt(releasesIn) : null;
  return (
    <div className={cx('kv-qrow', selected && 'is-selected')}>
      <div className="kv-qrow-line">
        {selectable && <Checkbox label="" checked={selected} onCheckedChange={onSelect} />}
        <button type="button" className="kv-qrow-main" onClick={onToggle} aria-expanded={expanded}>
          <span className="kv-qrow-route">
            <AgentAvatar {...from} size={24} /><Icon name="arrow-right" size={14} />
            <AgentAvatar {...to[0]} size={24} />{to.length > 1 && <span className="kv-qrow-plus">+{to.length - 1}</span>}
          </span>
          <span className="kv-qrow-text">
            <span className="kv-qrow-title">{title}</span>
            <span className="kv-qrow-meta">{from.name} → {to.map((t) => t.name).join(', ')}<Badge variant="outline" mono>{kind === 'assignment' ? 'assignment #' + refId : 'notice'}</Badge></span>
          </span>
        </button>
        <span className="kv-qrow-trail">{countdown && <span className="kv-qrow-count">{countdown}</span>}<Button size="sm" variant="secondary" onClick={onRelease}>Release</Button></span>
      </div>
      {expanded && <div className="kv-qrow-body">{children}</div>}
    </div>
  );
}
