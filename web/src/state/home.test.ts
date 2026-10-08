import { describe, expect, it } from 'vitest';
import type { QueueItem } from '../api/types.gen';
import { busyHome, goHome } from './fixtures/home';
import { quietSnapshot } from './fixtures';
import {
  asSentence,
  blockerText,
  effectiveSelection,
  filterQueue,
  formatMoney,
  formatSize,
  hasRunningCountdown,
  hide,
  needKindLabel,
  pruneHidden,
  queueCountdown,
  queueCountLabel,
  refetchDecision,
  releaseAllPlan,
  routerPath,
  toggleSelected,
  unhide,
  visibleQueue,
} from './home';
import { initialOrgState, reduceSnapshot } from './org';
import type { OrgState } from './org';

const ready: OrgState = reduceSnapshot(initialOrgState, quietSnapshot);

function queueItem(over: Partial<QueueItem>): QueueItem {
  const base = goHome.queue[0];
  if (!base) throw new Error('fixture has no queue');
  return { ...base, ...over };
}

describe('refetchDecision', () => {
  it('treats the first snapshot as the baseline', () => {
    expect(refetchDecision(initialOrgState, ready, false)).toEqual({ refetch: false, armed: false });
  });

  it('does nothing for the same state or before the org is ready', () => {
    expect(refetchDecision(ready, ready, false).refetch).toBe(false);
    expect(refetchDecision(initialOrgState, { ...initialOrgState }, false).refetch).toBe(false);
  });

  it('refetches when the queue, the inbox, messages or the tracker move', () => {
    expect(refetchDecision(ready, { ...ready, pendingPaths: [...ready.pendingPaths, 'messages/x.md'] }, false).refetch).toBe(true);
    expect(refetchDecision(ready, { ...ready, ceoPaths: ['messages/y.md'] }, false).refetch).toBe(true);
    expect(refetchDecision(ready, { ...ready, messagesTotal: ready.messagesTotal + 1 }, false).refetch).toBe(true);
    expect(refetchDecision(ready, { ...ready, assignmentsVersion: ready.assignmentsVersion + 1 }, false).refetch).toBe(true);
  });

  it('ignores snapshots that change nothing Home draws', () => {
    expect(refetchDecision(ready, { ...ready, running: !ready.running }, false).refetch).toBe(false);
  });

  it('refetches when an agent starts or stops needing help', () => {
    const tree = ready.tree.map((n, i) => (i === 0 ? { ...n, state: 'needs_help' as const } : n));
    expect(tree.length).toBeGreaterThan(0);
    expect(refetchDecision(ready, { ...ready, tree }, false).refetch).toBe(true);
  });

  // The server makes a "not connected" needs-help row for a disconnected
  // agent; when its pod connects, Home must drop the row at once.
  it('refetches when an agent connects or disconnects', () => {
    const off = { ...ready, tree: ready.tree.map((n, i) => (i === 0 ? { ...n, state: 'disconnected' as const } : n)) };
    expect(refetchDecision(ready, off, false).refetch).toBe(true);
    expect(refetchDecision(off, ready, false).refetch).toBe(true);
  });

  it('refetches on an auto-release change and once more on the next update', () => {
    const moved = { ...ready, autoRelease: 'off' as const };
    expect(refetchDecision(ready, moved, false)).toEqual({ refetch: true, armed: true });
    const confirmed = { ...moved };
    expect(refetchDecision(moved, confirmed, true)).toEqual({ refetch: true, armed: false });
    expect(refetchDecision(confirmed, { ...confirmed }, false)).toEqual({ refetch: false, armed: false });
  });
});

describe('queueCountdown', () => {
  const now = Date.parse('2026-09-20T17:00:00Z');

  it('counts whole seconds to the release time', () => {
    expect(queueCountdown(queueItem({ releases_at: '2026-09-20T17:00:30Z', held: false }), '30s', now)).toEqual({ held: false, releasesIn: 30 });
    expect(queueCountdown(queueItem({ releases_at: '2026-09-20T17:00:29.200Z', held: false }), '30s', now).releasesIn).toBe(30);
  });

  it('stops at zero once due', () => {
    expect(queueCountdown(queueItem({ releases_at: '2026-09-20T16:59:00Z', held: false }), '30s', now)).toEqual({ held: false, releasesIn: 0 });
  });

  it('is held when auto-release is Off, when the server says so, or with no release time', () => {
    expect(queueCountdown(queueItem({ releases_at: '2026-09-20T17:00:30Z', held: false }), 'off', now)).toEqual({ held: true });
    expect(queueCountdown(queueItem({ held: true }), '30s', now)).toEqual({ held: true });
    const { releases_at: _drop, ...noTime } = queueItem({ held: false });
    expect(queueCountdown(noTime, '30s', now)).toEqual({ held: true });
  });

  it('knows when the clock needs to run', () => {
    expect(hasRunningCountdown(goHome.queue, '30s')).toBe(true);
    expect(hasRunningCountdown(goHome.queue, 'off')).toBe(false);
    expect(hasRunningCountdown(goHome.queue.filter((q) => q.held), '30s')).toBe(false);
  });
});

describe('filter and selection', () => {
  it('filters by kind', () => {
    expect(filterQueue(busyHome.queue, 'all')).toHaveLength(3);
    expect(filterQueue(busyHome.queue, 'assignment').map((q) => q.kind)).toEqual(['assignment']);
    expect(filterQueue(busyHome.queue, 'notice').every((q) => q.kind === 'notice')).toBe(true);
  });

  it('selects only while auto-release is Off, in row order, ignoring rows that left', () => {
    const [a, b, c] = busyHome.queue.map((q) => q.path) as [string, string, string];
    let sel = toggleSelected(new Set(), c, true);
    sel = toggleSelected(sel, a, true);
    sel = toggleSelected(sel, 'messages/gone.md', true);
    expect(effectiveSelection(sel, busyHome.queue, 'off')).toEqual([a, c]);
    expect(effectiveSelection(sel, busyHome.queue, '30s')).toEqual([]);
    expect(toggleSelected(sel, b, false)).toBe(sel);
    expect(effectiveSelection(toggleSelected(sel, a, false), busyHome.queue, 'off')).toEqual([c]);
  });

  it('plans Release all: the selection, or the rows on screen, always by path', () => {
    const paths = busyHome.queue.map((q) => q.path);
    const first = paths[0] ?? '';
    expect(releaseAllPlan([first], busyHome.queue)).toEqual({ body: { paths: [first] }, paths: [first], label: 'Release 1 selected' });
    const notices = filterQueue(busyHome.queue, 'notice');
    expect(releaseAllPlan([], notices).body).toEqual({ paths: notices.map((q) => q.path) });
    // Never the server's "everything queued": a message that landed since the last refetch is not released unseen.
    expect(releaseAllPlan([], busyHome.queue)).toEqual({ body: { paths }, paths, label: 'Release all' });
  });
});

describe('optimistic removal', () => {
  it('hides a row, brings it back on error, and forgets it once the server drops it', () => {
    const [a, b] = goHome.queue.map((q) => q.path) as [string, string];
    const hidden = hide(new Set(), [a]);
    expect(visibleQueue(goHome.queue, hidden).map((q) => q.path)).toEqual([b]);
    expect(visibleQueue(goHome.queue, unhide(hidden, [a])).map((q) => q.path)).toEqual([a, b]);
    expect(pruneHidden(hidden, [a, b])).toBe(hidden);
    expect(pruneHidden(hidden, [b]).size).toBe(0);
  });
});

describe('words', () => {
  it('labels need kinds neutrally', () => {
    expect(busyHome.needs.map(needKindLabel)).toEqual(['Approval', 'Hire', 'Notice', 'Assigned to you', 'Needs help']);
  });

  it('says the blocker in words', () => {
    const goal = goHome.goals[0];
    if (!goal) throw new Error('fixture has no goal');
    expect(blockerText(goal)).toBe('Waiting on #45, Run the tests');
    expect(blockerText({ ...goal, blocked: [] })).toBeUndefined();
    expect(blockerText({ ...goal, blocked: [...goal.blocked, { id: 43, on: 46, on_title: 'Label' }] })).toBe('Waiting on #45, Run the tests and 1 more');
  });

  it('formats money, sizes, counts and messages', () => {
    expect(formatMoney(4.25)).toBe('$4.25');
    expect(formatMoney(1840)).toBe('$1,840.00');
    expect(formatSize(86016)).toBe('84 KB');
    expect(formatSize(0)).toBe('');
    expect(queueCountLabel(0, '30s')).toBe('empty');
    expect(queueCountLabel(8, 'off')).toBe('8 held');
    expect(queueCountLabel(2, '30s')).toBe('2 passing through');
    expect(asSentence('that request is no longer on file')).toBe('That request is no longer on file.');
  });

  it('turns server app paths into router paths', () => {
    expect(routerPath('/base/proposals/messages/x.md', '/base')).toBe('/proposals/messages/x.md');
    expect(routerPath('/messages/x.md', '/base')).toBe('/messages/x.md');
    expect(routerPath('/team', '')).toBe('/team');
  });
});
