import { useState } from 'react';
import type { AutoRelease as AutoReleaseValue, QueueItem } from '../../api/types.gen';
import { AutoRelease, Button, EmptyState, QueueRow, Text, TextField } from '../../ds';
import { QUEUE_FILTERS, queueCountdown, queueCountLabel, releaseAllPlan, selectionEnabled } from '../../state/home';
import type { QueueFilter } from '../../state/home';
import { autoReleaseLabel, autoReleaseValue } from '../../state/org';
import { Attachments, Body, useIdentify } from './parts';

interface QueueEntryProps {
  item: QueueItem;
  autoRelease: AutoReleaseValue;
  now: number;
  expanded: boolean;
  selected: boolean;
  onToggle(): void;
  onSelect(on: boolean): void;
  onRelease(note?: string): void;
  onBounce(comment: string): void;
}

function QueueEntry({ item, autoRelease, now, expanded, selected, onToggle, onSelect, onRelease, onBounce }: QueueEntryProps) {
  const [note, setNote] = useState('');
  const trimmed = note.trim();
  const identify = useIdentify();
  const countdown = queueCountdown(item, autoRelease, now);
  const kind = item.kind === 'assignment' ? 'assignment' : 'notice';
  return (
    <div className="app-home-item" data-path={item.path}>
      <QueueRow
        from={identify(item.from)}
        to={item.to.map(identify)}
        kind={kind}
        refId={item.assignment?.id ?? ''}
        title={item.title}
        held={countdown.held}
        releasesIn={countdown.releasesIn}
        selectable={selectionEnabled(autoRelease)}
        selected={selected}
        onSelect={(v) => onSelect(v === true)}
        onRelease={() => onRelease()}
        onToggle={onToggle}
        expanded={expanded}
      >
        <Body markdown={item.body_md} />
        <Attachments items={item.attachments} />
        <TextField
          label="Note to the recipient (optional)"
          hint="Delivered as top-priority direction from you."
          multiline
          rows={2}
          placeholder="Agreed. Keep the tier names as they are for now."
          value={note}
          onChange={(e) => setNote(e.currentTarget.value)}
        />
        <div className="app-home-actions">
          {/* eslint-disable-next-line react/forbid-elements -- external: the raw message is served outside the app */}
          <a className="app-home-link" href={item.raw_url} target="_blank" rel="noopener noreferrer">
            Raw message
          </a>
          <span className="app-home-spacer" />
          {kind === 'notice' && (
            <Button variant="ghost" size="sm" disabled={!trimmed} onClick={() => onBounce(trimmed)}>
              Bounce with note
            </Button>
          )}
          <Button variant="secondary" size="sm" onClick={() => onRelease(trimmed || undefined)}>
            Release
          </Button>
        </div>
      </QueueRow>
    </div>
  );
}

export interface QueueProps {
  items: readonly QueueItem[];
  /** How many are queued before the filter, for the count line. */
  total: number;
  autoRelease: AutoReleaseValue;
  onAutoRelease(value: AutoReleaseValue): void;
  /** The slider on desktop, the Select on phone. */
  compact: boolean;
  filter: QueueFilter;
  onFilter(f: QueueFilter): void;
  selection: readonly string[];
  now: number;
  expanded: ReadonlySet<string>;
  onToggle(path: string): void;
  onSelect(path: string, on: boolean): void;
  onRelease(path: string, note?: string): void;
  onReleaseAll(): void;
  onBounce(path: string, comment: string): void;
}

/** Messages between agents on their way: auto-release, the filter, Release all, and one row per message. */
export function Queue(p: QueueProps) {
  const plan = releaseAllPlan(p.selection, p.items);
  const chosen = new Set(p.selection);
  return (
    <section className="app-home-section" aria-labelledby="home-queue">
      <div className="app-home-queue-head">
        <div className="app-home-head">
          <Text as="h2" variant="heading" id="home-queue">
            Queue
          </Text>
          <Text variant="label" tone="muted">
            {queueCountLabel(p.total, p.autoRelease)}
          </Text>
        </div>
        <AutoRelease
          value={autoReleaseLabel(p.autoRelease)}
          variant={p.compact ? 'select' : 'slider'}
          onChange={(label) => {
            const value = autoReleaseValue(label);
            if (value) p.onAutoRelease(value);
          }}
        />
      </div>
      <div className="app-home-queue-tools">
        <div className="app-home-filters" role="group" aria-label="Show">
          {QUEUE_FILTERS.map((f) => (
            <Button key={f.value} size="sm" variant={p.filter === f.value ? 'secondary' : 'ghost'} aria-pressed={p.filter === f.value} onClick={() => p.onFilter(f.value)}>
              {f.label}
            </Button>
          ))}
        </div>
        <span className="app-home-release-all">
          {p.selection.length > 0 && (
            <Text variant="label" tone="muted">
              {p.selection.length} selected
            </Text>
          )}
          {p.items.length > 0 && (
            <Button variant="primary" onClick={p.onReleaseAll}>
              {plan.label}
            </Button>
          )}
        </span>
      </div>
      <div className="app-home-list">
        <div className="app-home-rows">
          {p.total === 0 ? (
            <EmptyState title="No deliveries queued">Messages between agents pass through here on their way.</EmptyState>
          ) : p.items.length === 0 ? (
            <Text as="p" variant="caption" tone="muted" className="app-home-none">
              {p.filter === 'assignment' ? 'No assignments queued.' : 'No notices queued.'}
            </Text>
          ) : (
            p.items.map((q) => (
              <QueueEntry
                key={q.path}
                item={q}
                autoRelease={p.autoRelease}
                now={p.now}
                expanded={p.expanded.has(q.path)}
                selected={chosen.has(q.path)}
                onToggle={() => p.onToggle(q.path)}
                onSelect={(on) => p.onSelect(q.path, on)}
                onRelease={(note) => p.onRelease(q.path, note)}
                onBounce={(comment) => p.onBounce(q.path, comment)}
              />
            ))
          )}
        </div>
      </div>
    </section>
  );
}
