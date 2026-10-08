import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import { Link, useHref, useSearchParams } from 'react-router';
import { apiGet } from '../../api/client';
import type { ApiError } from '../../api/client';
import type { ClosedGoal, PersonRef, UnclaimedCondition, WorkBoard, WorkGoal, WorkItem, WorkReadouts } from '../../api/types.gen';
import { useFrameChrome } from '../../app/chrome';
import { PageTitle } from '../../app/PageTitle';
import { Banner, Button, Card, EmptyState, AssignmentRef, AssignmentRow, Progress, Readouts, Skeleton, Text } from '../../ds';
import type { ReadoutItem } from '../../ds';
import { asSentence } from '../../state/home';
import { useOrg } from '../../state/OrgProvider';
import { CompactState, YOU, asApiError, readiness, useFaces, useSlow } from './parts';
import type { Faces } from './parts';
import '../../styles/work.css';

// The router mounts both screens of this folder from here.
export { Assignment } from './Assignment';

/** The ?state= values the readouts link to. */
const STATE_WORDS: Record<string, string> = {
  ready: 'ready',
  moving: 'moving',
  blocked: 'blocked',
  on_hold: 'on hold',
  closed: 'closed',
};

export function readoutItems(r: WorkReadouts, base: string): ReadoutItem[] {
  return [
    { n: String(r.open), label: 'open assignments', href: base + '/work' },
    { n: String(r.ready), label: 'ready', href: base + '/work?state=ready' },
    { n: String(r.blocked), label: 'blocked', href: base + '/work?state=blocked' },
    { n: String(r.on_hold), label: 'on hold', href: base + '/work?state=on_hold' },
    { n: String(r.closed_week), label: 'closed this week', href: base + '/work?closed=week' },
  ];
}

interface OwnerGroup {
  owner: PersonRef;
  items: WorkItem[];
}

/** Items grouped under their owner, in the order each owner first appears. */
export function groupByOwner(items: readonly WorkItem[]): OwnerGroup[] {
  const groups = new Map<string, OwnerGroup>();
  for (const item of items) {
    const g = groups.get(item.owner.slug);
    if (g) g.items.push(item);
    else groups.set(item.owner.slug, { owner: item.owner, items: [item] });
  }
  return [...groups.values()];
}

/** "On hold by Maya" without its lead: what AssignmentRow's heldBy prop expects. */
function heldByText(item: WorkItem): string | undefined {
  if (item.state !== 'on_hold') return undefined;
  const fromWhy = /^On hold by (.+)$/.exec(item.why ?? '');
  if (fromWhy?.[1]) return fromWhy[1];
  if (item.held_by?.slug === YOU) return 'you';
  return item.held_by?.name || 'someone';
}

function Row({ item, base }: { item: WorkItem; base: string }) {
  const held = heldByText(item);
  return (
    <AssignmentRow
      id={item.id}
      title={item.title}
      state={readiness(item.state, item.resolution)}
      href={base + '/assignments/' + item.id}
      // The reason in words: each ref with its title, "Waiting on #45, Run the tests".
      {...(item.state === 'blocked' && item.waiting_on.length > 0 ? { waitingOn: item.waiting_on.map((w) => ({ id: w.id, state: readiness(w.state), title: w.title })) } : {})}
      {...(held ? { heldBy: held } : {})}
      {...(item.acceptance ? { acceptance: item.acceptance } : {})}
    />
  );
}

interface ColumnProps {
  label: string;
  sub: string;
  items: readonly WorkItem[];
  unclaimed?: readonly UnclaimedCondition[];
  empty: string;
  base: string;
  faces: Faces;
}

function Column({ label, sub, items, unclaimed = [], empty, base, faces }: ColumnProps) {
  const groups = groupByOwner(items);
  const headingId = useId();
  return (
    <section className="app-work-col" aria-labelledby={headingId}>
      <div className="app-work-col-head">
        <Text as="h3" variant="label" className="app-work-col-label" id={headingId}>
          {label}
        </Text>
        <Text variant="label" tone="muted">
          {sub}
        </Text>
      </div>
      <div className="app-work-box">
        {groups.map((g) => (
          <div key={g.owner.slug} className="app-work-group">
            <div className="app-work-owner">
              {faces.face(g.owner, 20)}
              <span>{g.owner.name}</span>
              <CompactState node={faces.node(g.owner.slug)} />
            </div>
            {g.items.map((item) => (
              <Row key={item.id} item={item} base={base} />
            ))}
          </div>
        ))}
        {unclaimed.map((u) => (
          <p key={u.name} className="app-work-unclaimed">
            <span className="app-work-unclaimed-name">{u.name}</span>
            <span className="app-work-unclaimed-word">Unclaimed</span>
          </p>
        ))}
        {groups.length === 0 && unclaimed.length === 0 && (
          <Text as="p" variant="caption" tone="muted" className="app-work-empty">
            {empty}
          </Text>
        )}
      </div>
    </section>
  );
}

/** "2 moving · 1 blocked" for the Current column. */
function currentSub(items: readonly WorkItem[]): string {
  const parts: string[] = [];
  for (const state of ['moving', 'ready', 'blocked', 'on_hold']) {
    const n = items.filter((i) => i.state === state).length;
    if (n > 0) parts.push(n + ' ' + (STATE_WORDS[state] ?? state));
  }
  return parts.length > 0 ? parts.join(' · ') : 'none';
}

function GoalBlock({ goal, base, faces, filter }: { goal: WorkGoal; base: string; faces: Faces; filter: string | null }) {
  const pick = (items: readonly WorkItem[]) => (filter ? items.filter((i) => i.state === filter) : items);
  const back = pick(goal.look_back);
  const current = pick(goal.current);
  const forward = pick(goal.look_forward);
  // A filter narrows the board to items; the unclaimed conditions are not items, so they wait for the full board.
  const unclaimed = filter ? [] : goal.unclaimed;
  const id = 'work-goal-' + goal.id;
  return (
    <section className="app-work-goal" aria-labelledby={id}>
      <div className="app-work-goal-head">
        <div className="app-work-goal-titles">
          <div className="app-work-goal-line">
            <Text as="h2" variant="heading" id={id}>
              <Link className="app-work-goal-link" to={'/assignments/' + goal.id}>
                {goal.title}
              </Link>
            </Text>
            <AssignmentRef id={goal.id} />
          </div>
          <div className="app-work-goal-owner">
            {faces.face(goal.owner, 24)}
            <Text variant="caption" tone="muted">
              {goal.owner.name}
            </Text>
          </div>
        </div>
        <div className="app-work-goal-progress">
          <div className="app-work-goal-bar">
            <Progress value={goal.done} max={Math.max(goal.total, 1)} aria-label={`${goal.done} of ${goal.total}`} />
          </div>
          <Text variant="label" tone="muted" className="app-work-nowrap">
            {goal.done} of {goal.total}
          </Text>
        </div>
      </div>
      <div className="app-work-cols">
        <Column label="Look back" sub={'closed this week · ' + back.length} items={back} empty="Nothing closed this week" base={base} faces={faces} />
        <Column label="Current" sub={currentSub(current)} items={current} empty="Nothing in progress" base={base} faces={faces} />
        <Column
          label="Look forward"
          sub={'ready' + (unclaimed.length > 0 ? ' · ' + unclaimed.length + ' unclaimed' : '')}
          items={forward}
          unclaimed={unclaimed}
          empty="Nothing queued"
          base={base}
          faces={faces}
        />
      </div>
    </section>
  );
}

function ClosedGoals({ goals, open, base, faces }: { goals: readonly ClosedGoal[]; open: boolean; base: string; faces: Faces }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (open) ref.current?.scrollIntoView({ block: 'start' });
  }, [open]);
  return (
    <div ref={ref} id="closed-this-week">
      <Card collapsible defaultOpen={open} title="Recently closed goals" meta={goals.length + ' this week'}>
        <div className="app-work-closed">
          {goals.length === 0 ? (
            <Text as="p" variant="caption" tone="muted" className="app-work-empty">
              No goals closed this week.
            </Text>
          ) : (
            goals.map((g) => (
              <AssignmentRow key={g.id} id={g.id} title={g.title} state={readiness('closed', g.resolution)} assignee={faces.assignee(g.owner)} href={base + '/assignments/' + g.id} />
            ))
          )}
        </div>
      </Card>
    </div>
  );
}

function BoardSkeleton() {
  return (
    <div className="app-work-loading" aria-busy="true" aria-label="Loading Work">
      {[0, 1].map((i) => (
        <div key={i} className="app-work-goal">
          <Skeleton width="40%" height={20} />
          <div className="app-work-cols">
            {[0, 1, 2].map((c) => (
              <div key={c} className="app-work-box app-work-skeleton">
                <Skeleton width="70%" />
                <Skeleton width="45%" height={12} />
              </div>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}

/** Work: the board by goal. Readouts link to filtered views (?state=, ?closed=week); nothing on it files work. */
export function Work() {
  useFrameChrome({ title: 'Work', width: 'wide' });
  const { org } = useOrg();
  const faces = useFaces();
  const base = useHref('/').replace(/\/$/, '');
  const [params] = useSearchParams();
  const stateParam = params.get('state');
  const filter = stateParam && stateParam in STATE_WORDS ? stateParam : null;
  const closedWeek = params.get('closed') === 'week';

  const [board, setBoard] = useState<WorkBoard | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const seq = useRef(0);
  const load = useCallback(async () => {
    const mine = ++seq.current;
    try {
      const next = await apiGet<WorkBoard>('/api/v1/work');
      if (mine !== seq.current) return;
      setBoard(next);
      setError(null);
    } catch (err) {
      if (mine !== seq.current) return;
      setError(asApiError(err));
    }
  }, []);

  useEffect(() => {
    void load();
    return () => {
      // A response that lands after unmount is dropped.
      seq.current++;
    };
  }, [load]);

  // The board follows the tracker: a new assignments_version in the org snapshot means something changed.
  const seenVersion = useRef<number | null>(null);
  useEffect(() => {
    if (!org.ready) return;
    if (seenVersion.current === null) {
      seenVersion.current = org.assignmentsVersion;
      return;
    }
    if (seenVersion.current !== org.assignmentsVersion) {
      seenVersion.current = org.assignmentsVersion;
      void load();
    }
  }, [org.ready, org.assignmentsVersion, load]);

  const slow = useSlow(board === null && !error);
  const visibleGoals = useMemo(() => {
    if (!board) return [];
    if (!filter) return board.goals;
    return board.goals.filter((g) => [...g.look_back, ...g.current, ...g.look_forward].some((i) => i.state === filter));
  }, [board, filter]);

  return (
    <div className="app-work">
      <div className="app-work-top">
        <PageTitle>Work</PageTitle>
        {board && <Readouts items={readoutItems(board.readouts, base)} />}
      </div>
      {error && (
        <Banner
          tone="danger"
          title={asSentence(error.message)}
          action={
            <Button size="sm" variant="secondary" onClick={() => void load()}>
              Try again
            </Button>
          }
        >
          {error.who}
        </Banner>
      )}
      {board === null && !error && slow && <BoardSkeleton />}
      {board && (
        <>
          {filter && (
            <div className="app-work-filter">
              <Text variant="caption" tone="muted">
                Showing {STATE_WORDS[filter]} assignments only.
              </Text>
              <Link className="app-work-link" to="/work">
                Show all
              </Link>
            </div>
          )}
          {board.goals.length === 0 ? (
            <EmptyState title="No goals yet">Goals are the top-level assignments your team works toward. Ask an agent to open one and it appears here.</EmptyState>
          ) : visibleGoals.length === 0 ? (
            <Text as="p" variant="body" tone="muted">
              Nothing is {STATE_WORDS[filter ?? ''] ?? 'in that state'} right now.
            </Text>
          ) : (
            visibleGoals.map((g) => <GoalBlock key={g.id} goal={g} base={base} faces={faces} filter={filter} />)
          )}
          {(board.closed_goals.length > 0 || closedWeek) && <ClosedGoals goals={board.closed_goals} open={closedWeek} base={base} faces={faces} />}
        </>
      )}
    </div>
  );
}
