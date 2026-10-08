import { useMemo } from 'react';
import type { Goal, Readouts as ReadoutsData } from '../../api/types.gen';
import { runStateView } from '../../app/runState';
import { GoalRow, Readouts, Text } from '../../ds';
import type { GoalWorker, ReadoutItem } from '../../ds';
import { blockerText, formatMoney } from '../../state/home';
import { flattenTree } from '../../state/org';
import type { TreeNode } from '../../state/org';
import { YOU, useIdentify } from './parts';

export interface InFlightProps {
  goals: readonly Goal[];
  readouts: ReadoutsData;
  tree: TreeNode[];
  /** The app's base path ("" at the root), for the plain hrefs Readouts and GoalRow render. */
  base: string;
  /** 3 on desktop, 2 on phone. */
  maxWorkers: number;
}

export function readoutItems(r: ReadoutsData, base: string): ReadoutItem[] {
  return [
    { n: String(r.working), label: r.working === 1 ? 'agent working' : 'agents working', href: base + '/team' },
    { n: String(r.blocked), label: 'blocked', href: base + '/work?state=blocked' },
    { n: String(r.closed_week), label: 'closed this week', href: base + '/work?closed=week' },
    { n: formatMoney(r.spend_today), label: 'today', href: base + '/org' },
    { n: formatMoney(r.spend_7d), label: '7d', href: base + '/org' },
  ];
}

/** Goals and progress, not who is doing what: the readouts line, then one row per goal. */
export function InFlight({ goals, readouts, tree, base, maxWorkers }: InFlightProps) {
  const agents = useMemo(() => new Map(flattenTree(tree).map((n) => [n.slug, n])), [tree]);
  const identify = useIdentify();
  const face = (slug: string) => identify({ slug, name: agents.get(slug)?.name ?? slug });
  const worker = (slug: string): GoalWorker => {
    const node = agents.get(slug);
    const view = node ? runStateView(node.state, node.waitingTasks) : null;
    return { ...face(slug), state: view?.state ?? 'idle' };
  };
  // GoalRow draws agents only: a part assigned to you is not "working", and a goal you own shows no owner tile.
  const agentSlugs = (slugs: readonly string[]) => slugs.filter((s) => s !== YOU);

  return (
    <section className="app-home-section" aria-labelledby="home-in-flight">
      <div className="app-home-head app-home-readouts-head">
        <Text as="h2" variant="heading" id="home-in-flight">
          In flight
        </Text>
        <Readouts items={readoutItems(readouts, base)} />
      </div>
      <div className="app-home-list">
        <div className="app-home-rows">
          {goals.length === 0 ? (
            <Text as="p" variant="caption" tone="muted" className="app-home-none">
              No goals in flight. Goals are the top-level assignments your team works toward.
            </Text>
          ) : (
            goals.map((g) => (
              <GoalRow
                key={g.id}
                title={g.title}
                href={base + '/assignments/' + g.id}
                {...(g.owner && g.owner !== YOU ? { owner: face(g.owner) } : {})}
                done={g.done}
                total={g.total}
                blocker={blockerText(g)}
                workers={agentSlugs(g.workers).map(worker)}
                maxWorkers={maxWorkers}
              />
            ))
          )}
        </div>
      </div>
    </section>
  );
}
