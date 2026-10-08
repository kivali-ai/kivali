// Pure helpers behind the Home screen: when to refetch /api/v1/home, the queue countdowns, the filter, the
// selection, and hiding a row the moment it is acted on (with rollback when the server refuses).
import type { AutoRelease, Goal, NeedItem, QueueItem, QueueReleaseAllRequest } from '../api/types.gen';
import { flattenTree } from './org';
import type { OrgState } from './org';

// ---- Refetch ----

function sameList(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

/** Agents a needs-help row can be about (the server also makes one for a
 *  disconnected agent). Their coming and going changes Needs you without
 *  any inbox change. */
export function needsHelpSlugs(org: OrgState): string[] {
  return flattenTree(org.tree)
    .filter((n) => n.state === 'needs_help' || n.state === 'quarantined' || n.state === 'disconnected')
    .map((n) => n.slug)
    .sort();
}

/** True when a snapshot moved something /api/v1/home draws: the queue, the inbox, messages, the tracker, or who needs help. */
export function homeKeysChanged(prev: OrgState, next: OrgState): boolean {
  return (
    !sameList(prev.pendingPaths, next.pendingPaths) ||
    !sameList(prev.ceoPaths, next.ceoPaths) ||
    prev.messagesTotal !== next.messagesTotal ||
    prev.assignmentsVersion !== next.assignmentsVersion ||
    !sameList(needsHelpSlugs(prev), needsHelpSlugs(next))
  );
}

export interface RefetchDecision {
  refetch: boolean;
  /**
   * Set after an auto-release change: the next org update refetches once more. The stop can change locally
   * (the slider, before the server has it), and the queue's new release times exist only once the server has
   * written the setting, which the snapshot after the write announces.
   */
  armed: boolean;
}

/**
 * Whether Home refetches after the org state moved from `prev` to `next`. The first snapshot is the
 * baseline (Home loads on mount), so it never refetches.
 */
export function refetchDecision(prev: OrgState, next: OrgState, armed: boolean): RefetchDecision {
  if (prev === next || !next.ready) return { refetch: false, armed };
  if (!prev.ready) return { refetch: false, armed: false };
  if (prev.autoRelease !== next.autoRelease) return { refetch: true, armed: true };
  if (armed) return { refetch: true, armed: false };
  return { refetch: homeKeysChanged(prev, next), armed: false };
}

// ---- Countdowns ----

export interface Countdown {
  held: boolean;
  /** Whole seconds until the message releases; 0 once it is due. Absent when held. */
  releasesIn?: number;
}

/**
 * A queued message's countdown at `now` (ms). Held when auto-release is Off (at once, before the refetch
 * brings the server's view), when the server says held, or when there is no release time.
 */
export function queueCountdown(item: QueueItem, autoRelease: AutoRelease, now: number): Countdown {
  if (autoRelease === 'off' || item.held || !item.releases_at) return { held: true };
  const at = Date.parse(item.releases_at);
  if (Number.isNaN(at)) return { held: true };
  return { held: false, releasesIn: Math.max(0, Math.ceil((at - now) / 1000)) };
}

/** Whether any row is counting down, so the one-second clock needs to run. */
export function hasRunningCountdown(items: readonly QueueItem[], autoRelease: AutoRelease): boolean {
  return autoRelease !== 'off' && items.some((q) => !q.held && !!q.releases_at);
}

// ---- Filter ----

export type QueueFilter = 'all' | 'assignment' | 'notice';

export const QUEUE_FILTERS: readonly { value: QueueFilter; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'assignment', label: 'Assignments' },
  { value: 'notice', label: 'Notices' },
];

export function filterQueue(items: readonly QueueItem[], filter: QueueFilter): QueueItem[] {
  return filter === 'all' ? [...items] : items.filter((q) => q.kind === filter);
}

// ---- Selection (auto-release Off only) ----

export function selectionEnabled(autoRelease: AutoRelease): boolean {
  return autoRelease === 'off';
}

export function toggleSelected(selected: ReadonlySet<string>, path: string, on: boolean): ReadonlySet<string> {
  if (selected.has(path) === on) return selected;
  const next = new Set(selected);
  if (on) next.add(path);
  else next.delete(path);
  return next;
}

/** The selected paths still on screen, in row order; none unless auto-release is Off. */
export function effectiveSelection(selected: ReadonlySet<string>, visible: readonly QueueItem[], autoRelease: AutoRelease): string[] {
  if (!selectionEnabled(autoRelease)) return [];
  return visible.filter((q) => selected.has(q.path)).map((q) => q.path);
}

export interface ReleaseAllPlan {
  body: QueueReleaseAllRequest;
  /** The rows that leave at once. */
  paths: string[];
  label: string;
}

/**
 * What "Release all" sends: always the paths, never the server's "everything queued". A selection releases
 * exactly the selected rows the filter shows ("Release 3 selected"); otherwise the rows on screen go, which
 * under a filter is what the filter shows. A message that landed after the last refetch is not on screen, so
 * it is never released unseen (that matters most with auto-release Off, where the person moderates).
 */
export function releaseAllPlan(selection: readonly string[], visible: readonly QueueItem[]): ReleaseAllPlan {
  if (selection.length > 0) return { body: { paths: [...selection] }, paths: [...selection], label: 'Release ' + selection.length + ' selected' };
  const paths = visible.map((q) => q.path);
  return { body: { paths }, paths, label: 'Release all' };
}

// ---- Optimistic removal ----

export function hide(hidden: ReadonlySet<string>, ids: readonly string[]): ReadonlySet<string> {
  const next = new Set(hidden);
  for (const id of ids) next.add(id);
  return next;
}

/** Rollback: the rows come back where they were. */
export function unhide(hidden: ReadonlySet<string>, ids: readonly string[]): ReadonlySet<string> {
  if (!ids.some((id) => hidden.has(id))) return hidden;
  const next = new Set(hidden);
  for (const id of ids) next.delete(id);
  return next;
}

/** Forgets hidden ids the server no longer lists; ids still listed stay hidden until the server drops them. */
export function pruneHidden(hidden: ReadonlySet<string>, present: readonly string[]): ReadonlySet<string> {
  const on = new Set(present);
  const kept = [...hidden].filter((id) => on.has(id));
  return kept.length === hidden.size ? hidden : new Set(kept);
}

export function visibleNeeds(items: readonly NeedItem[], hidden: ReadonlySet<string>): NeedItem[] {
  return items.filter((n) => !hidden.has(n.id));
}

export function visibleQueue(items: readonly QueueItem[], hidden: ReadonlySet<string>): QueueItem[] {
  return items.filter((q) => !hidden.has(q.path));
}

// ---- Words ----

const PROPOSAL_LABELS: Record<string, string> = {
  hire: 'Hire',
  role_update: 'Role update',
  handbook_update: 'Handbook update',
  offboard: 'Offboard',
  reorg: 'Reorg',
};

/** The neutral kind badge on a Needs-you row. */
export function needKindLabel(item: NeedItem): string {
  switch (item.kind) {
    case 'approval':
      return 'Approval';
    case 'notification':
      return 'Notice';
    case 'assignment':
      return 'Assigned to you';
    case 'needs_help':
      return 'Needs help';
    case 'proposal':
      return PROPOSAL_LABELS[item.proposal_kind ?? ''] ?? 'Proposal';
    default:
      return 'Request';
  }
}

/** The blocker in words: "Waiting on #42, Run the tests", with a count when a goal waits on more than one thing. */
export function blockerText(goal: Goal): string | undefined {
  const first = goal.blocked[0];
  if (!first) return undefined;
  const head = 'Waiting on #' + first.on + (first.on_title ? ', ' + first.on_title : '');
  const rest = goal.blocked.length - 1;
  return rest > 0 ? head + ' and ' + rest + ' more' : head;
}

export function formatMoney(dollars: number): string {
  return '$' + dollars.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

/** The mono line beside the Queue heading. */
export function queueCountLabel(count: number, autoRelease: AutoRelease): string {
  if (count === 0) return 'empty';
  return count + (autoRelease === 'off' ? ' held' : ' passing through');
}

/** A size for an attachment button: "84 KB". */
export function formatSize(bytes: number): string {
  if (bytes <= 0) return '';
  if (bytes < 1024) return bytes + ' B';
  if (bytes < 1024 * 1024) return Math.round(bytes / 1024) + ' KB';
  return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
}

/** A server error message as a sentence: capitalised, with a full stop. */
export function asSentence(message: string): string {
  const t = message.trim();
  if (!t) return t;
  const cap = t.charAt(0).toUpperCase() + t.slice(1);
  return /[.?]$/.test(cap) ? cap : cap + '.';
}

/** A server path under the app's base ("/base/proposals/x") as a router path ("/proposals/x"); unchanged when the app is at the root. */
export function routerPath(path: string, base: string): string {
  if (base && (path === base || path.startsWith(base + '/'))) return path.slice(base.length) || '/';
  return path;
}
