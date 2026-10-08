import { useEffect, useMemo, useRef, useState } from 'react';
import type { Background as BackgroundData, BackgroundSession, PlanStep } from '../../api/types.gen';
import { AgentState, Badge, Banner, Card, EmptyState, Icon, ListRow, Progress, Skeleton, Text } from '../../ds';
import { flattenTree } from '../../state/org';
import { useOrg } from '../../state/OrgProvider';
import { useNow } from '../home/useNow';
import { useAgentPage } from './AgentPage';
import { useChatDeps } from './chatDeps';
import { SESSION_STATE, STEP_STATE, elapsedLabel, modelLabel, useAppHref, useFetched } from './tabsShared';
import '../../styles/agent-tabs.css';

const isLive = (s: BackgroundSession) => s.state === 'running' || s.state === 'queued';

/**
 * The deepest level agent-tabs.css draws apart; a task deeper sits at it. The server allows two tiers of subagent
 * (agentpod.MaxSubagentDepth), so the list goes one level deep today.
 */
const DEEPEST = 3;

/**
 * One session placed in its group's tree. `depth` counts the tasks above it (0 for one the agent dispatched, or
 * one whose caller is not in the group); `rails[l]` says the branch at level l carries on past this row, so the
 * rail there runs through it; `last` says it is its caller's last task, so its branch ends in an elbow rather
 * than a tee; `parent` is the task that dispatched it.
 */
export interface PlacedSession {
  session: BackgroundSession;
  depth: number;
  rails: boolean[];
  last: boolean;
  hasChildren: boolean;
  parent?: BackgroundSession;
}

/**
 * The group's sessions depth first: each task straight after the one that dispatched it (`caller_id`), siblings
 * in the order the server sent them (oldest dispatch first). A session whose caller is not in the group heads a
 * tree of its own, and so does any caught in a caller loop.
 */
export function sessionTree(sessions: readonly BackgroundSession[]): PlacedSession[] {
  const ids = new Set(sessions.map((s) => s.id));
  const children = new Map<string, BackgroundSession[]>();
  const roots: BackgroundSession[] = [];
  for (const s of sessions) {
    if (s.caller_id && s.caller_id !== s.id && ids.has(s.caller_id)) children.set(s.caller_id, [...(children.get(s.caller_id) ?? []), s]);
    else roots.push(s);
  }
  const out: PlacedSession[] = [];
  const seen = new Set<string>();
  const walk = (s: BackgroundSession, rails: boolean[], last: boolean, parent: BackgroundSession | undefined) => {
    seen.add(s.id);
    const kids = (children.get(s.id) ?? []).filter((k) => !seen.has(k.id));
    kids.forEach((k) => seen.add(k.id));
    out.push({ session: s, depth: rails.length, rails, last, hasChildren: kids.length > 0, parent });
    kids.forEach((k, i) => walk(k, [...rails, i < kids.length - 1], i === kids.length - 1, s));
  };
  for (const r of roots) walk(r, [], true, undefined);
  // A caller loop has no root: its first session heads it.
  for (const s of sessions) if (!seen.has(s.id)) walk(s, [], true, undefined);
  return out;
}

/** "2 running · 1 done · 1 needs help", or what a step with no sessions says about itself. */
function stepMeta(step: PlanStep, sessions: readonly BackgroundSession[]): string {
  if (sessions.length === 0) return step.state === 'done' ? 'Done' : step.state === 'needs_help' ? 'Needs help' : 'Not started';
  const count = (test: (s: BackgroundSession) => boolean) => sessions.filter(test).length;
  const parts: string[] = [];
  const running = count((s) => s.state === 'running');
  const queued = count((s) => s.state === 'queued');
  const done = count((s) => s.state === 'done');
  const errored = count((s) => s.state === 'errored');
  if (running) parts.push(running + ' running');
  if (queued) parts.push(queued + ' queued');
  if (done) parts.push(done + ' done');
  if (errored) parts.push(errored + ' needs help');
  return parts.length ? parts.join(' · ') : sessions.length + (sessions.length === 1 ? ' session' : ' sessions');
}

/** "3 of 7 steps · 2 running · 9 finished · 1 needs help": the parts that are zero are left out. */
function PlanCounts({ plan }: { plan: NonNullable<BackgroundData['plan']> }) {
  const rest: string[] = [];
  if (plan.running > 0) rest.push(plan.running + ' running');
  if (plan.finished > 0) rest.push(plan.finished + ' finished');
  if (plan.needs_help > 0) rest.push(plan.needs_help + ' needs help');
  return (
    <Text as="p" variant="label" tone="muted" className="app-bg-counts">
      <span className="app-bg-steps">
        {plan.done} of {plan.total} {plan.total === 1 ? 'step' : 'steps'}
      </span>
      {rest.map((part) => (
        <span key={part}>
          {' '}
          <span aria-hidden="true">· </span>
          {part}
        </span>
      ))}
    </Text>
  );
}

/**
 * The tree lines left of a task: a rail through the row for each level above it whose branch carries on, the
 * elbow (a tee when a sibling follows) that joins it to its caller, and a stem down from its own state when tasks
 * of its own follow.
 */
function TaskRails({ place }: { place: PlacedSession }) {
  const { depth, rails, last, hasChildren } = place;
  const at = (kind: string, l: number) => kind + ' app-bg-level-' + Math.min(l, DEEPEST);
  return (
    <span className="app-bg-rails" aria-hidden="true">
      {rails.slice(0, -1).map((on, l) => (on ? <span key={l} className={at('app-bg-rail', l)} /> : null))}
      {depth > 0 && !last && <span className={at('app-bg-rail', depth - 1)} />}
      {depth > 0 && <span className={at('app-bg-elbow', depth - 1)} />}
      {hasChildren && <span className={at('app-bg-stem', depth)} />}
    </span>
  );
}

/** `sinceRead` is the seconds since the read that brought the session: a live task's clock carries on by it. */
function TaskRow({ place, slug, indent, sinceRead }: { place: PlacedSession; slug: string; indent: boolean; sinceRead: number }) {
  const href = useAppHref();
  const { session, depth, parent } = place;
  const live = isLive(session);
  const note = session.state === 'errored' && session.error ? session.error : live ? session.activity : undefined;
  const cls = ['app-bg-task', 'app-bg-depth-' + Math.min(depth, DEEPEST), indent && 'app-bg-task-nested', depth > 0 && 'app-bg-task-child'].filter(Boolean).join(' ');
  return (
    <div className={cls} data-depth={depth}>
      {(depth > 0 || place.hasChildren) && <TaskRails place={place} />}
      <ListRow
        lead={<AgentState state={SESSION_STATE[session.state]} compact />}
        title={
          <>
            {session.title}
            {parent && <span className="app-sr-only">{', a task of ' + parent.title}</span>}
          </>
        }
        meta={
          <>
            <Badge variant="outline" mono>
              {modelLabel(session.model, session.effort)}
            </Badge>
            {note && <span className={session.state === 'errored' ? 'app-bg-failure' : undefined}>{note}</span>}
          </>
        }
        trail={
          <>
            <span className="app-bg-elapsed">{elapsedLabel(session.elapsed_s + (live ? sinceRead : 0), live)}</span>
            <Icon name="chevron-right" />
          </>
        }
        href={href('/agents/' + encodeURIComponent(slug) + '/subagents/' + encodeURIComponent(session.id))}
      />
    </div>
  );
}

/**
 * The Background tab: what the agent has running off to the side. A plan, when it wrote one, heads the list and
 * its steps hold the tasks worked for them; tasks that belong to no step follow. The tab follows the org
 * snapshot: while the agent is working or has tasks out every snapshot
 * re-reads it (one read in flight, one trailing), and one more read follows the snapshot where that stops, so
 * the last task never sits there "running". A snapshot with nothing in flight reads nothing.
 *
 * Between reads a live task's clock counts up on its own. The server's elapsed time is as of the read, so the
 * row adds the time since that read arrived: the reader's clock only measures the gap, and being set wrong
 * does not show.
 */
export function Background() {
  const { slug, name } = useAgentPage();
  const { org } = useOrg();
  const deps = useChatDeps();
  const { data, error, loading, refresh } = useFetched<BackgroundData>('/api/v1/agents/' + encodeURIComponent(slug) + '/background');
  const [toggled, setToggled] = useState<Record<number, boolean>>({});
  const [read, setRead] = useState({ data, at: deps.now() });
  if (read.data !== data) setRead({ data, at: deps.now() });
  const now = useNow((data?.sessions ?? []).some(isLive) ? 1000 : 60_000, deps.now);
  const sinceRead = Math.max(0, (now - read.at) / 1000);

  const node = useMemo(() => flattenTree(org.tree).find((n) => n.slug === slug), [org.tree, slug]);
  // Work is in flight while the agent runs a turn (it may be writing the plan or dispatching) or while any of
  // its background tasks is still out, whatever state the agent itself shows.
  const live = node !== undefined && (node.state === 'running' || node.state === 'waiting' || node.waitingTasks > 0);
  const wasLive = useRef(live);
  const baselined = useRef(false);
  useEffect(() => {
    // The mount's own read stands for the first snapshot. Later snapshots re-read while work is live and once
    // more when it stops.
    if (!org.ready) return;
    if (!baselined.current) {
      baselined.current = true;
      wasLive.current = live;
      return;
    }
    if (live || wasLive.current) void refresh();
    wasLive.current = live;
  }, [org.ready, org.tree, live, refresh]);

  const { byStep, loose } = useMemo(() => {
    const steps = new Set((data?.plan?.steps ?? []).map((s) => s.n));
    const map = new Map<number, BackgroundSession[]>();
    const rest: BackgroundSession[] = [];
    for (const s of data?.sessions ?? []) {
      if (s.step !== undefined && steps.has(s.step)) map.set(s.step, [...(map.get(s.step) ?? []), s]);
      else rest.push(s);
    }
    return { byStep: new Map([...map].map(([n, list]) => [n, sessionTree(list)])), loose: sessionTree(rest) };
  }, [data]);

  if (loading) {
    return (
      <div className="app-bg" aria-busy="true">
        <Skeleton height={96} />
        <Skeleton height={56} />
        <Skeleton height={56} />
      </div>
    );
  }
  if (!data) {
    return (
      <Banner tone="danger" title={error?.message ?? 'The background work could not be read.'}>
        {error?.who}
      </Banner>
    );
  }

  const plan = data.plan;
  if (!plan && data.sessions.length === 0) {
    return (
      <div className="app-bg">
        {error && (
          <Banner tone="warning" title={error.message}>
            {error.who}
          </Banner>
        )}
        <div className="app-tabs-list">
          <EmptyState title="Nothing running in the background">{'Sessions ' + name + ' starts show up here while they run.'}</EmptyState>
        </div>
      </div>
    );
  }

  const running = data.sessions.filter(isLive).length;
  const finished = data.sessions.length - running;
  const noPlanLabel = ['No plan', running ? running + ' running' : '', finished ? finished + ' finished' : ''].filter(Boolean).join(' · ');

  return (
    <div className="app-bg">
      {error && (
        <Banner tone="warning" title={error.message}>
          {error.who}
        </Banner>
      )}
      {plan && (
        <Card title={plan.title}>
          <div className="app-bg-plan">
            <Progress value={plan.done} max={Math.max(plan.total, 1)} aria-label={`${plan.done} of ${plan.total}`} />
            <PlanCounts plan={plan} />
          </div>
        </Card>
      )}
      {plan && plan.steps.length > 0 && (
        <div className="app-tabs-list">
          <div className="app-tabs-rows">
            {plan.steps.map((step) => {
              const sessions = byStep.get(step.n) ?? [];
              const open = toggled[step.n] ?? step.state === 'running';
              const hasSessions = sessions.length > 0;
              return (
                <div key={step.n} className="app-bg-step">
                  <ListRow
                    lead={<AgentState state={STEP_STATE[step.state]} compact />}
                    title={step.n + ' · ' + step.title}
                    meta={stepMeta(step, sessions.map((p) => p.session))}
                    {...(hasSessions
                      ? {
                          trail: <Icon name={open ? 'chevron-up' : 'chevron-down'} />,
                          onClick: () => setToggled((t) => ({ ...t, [step.n]: !open })),
                          expanded: open,
                        }
                      : {})}
                  />
                  {open && sessions.map((p) => <TaskRow key={p.session.id} place={p} slug={slug} indent sinceRead={sinceRead} />)}
                </div>
              );
            })}
          </div>
        </div>
      )}
      {loose.length > 0 && (
        <div className="app-bg-group">
          <Text as="h2" variant="label" tone="muted">
            {plan ? 'Other background work' : noPlanLabel}
          </Text>
          <div className="app-tabs-list">
            <div className="app-tabs-rows">
              {loose.map((p) => (
                <TaskRow key={p.session.id} place={p} slug={slug} indent={false} sinceRead={sinceRead} />
              ))}
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
