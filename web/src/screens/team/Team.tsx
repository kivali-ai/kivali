import { useEffect, useMemo, useRef } from 'react';
import type { ReactNode } from 'react';
import type { AgentSummary, AgentsResponse } from '../../api/types.gen';
import { useFrameChrome } from '../../app/chrome';
import { PageTitle } from '../../app/PageTitle';
import { runStateView } from '../../app/runState';
import {
  AgentAvatar,
  AgentState,
  BREAKPOINT_TABBAR,
  Card,
  ContextCount,
  EmptyState,
  Icon,
  ListRow,
  PersonAvatar,
  Skeleton,
  Text,
  mediaQuery,
} from '../../ds';
import { agentIdentity } from '../../lib/agentIdentity';
import { shortDate } from '../../lib/time';
import { useMediaQuery } from '../../lib/useMediaQuery';
import { flattenTree } from '../../state/org';
import type { TreeNode } from '../../state/org';
import { useOrg } from '../../state/OrgProvider';
import { useChatDeps } from '../agent/chatDeps';
import { modelLabel, useAppHref, useFetched } from '../agent/tabsShared';
import '../../styles/team.css';

function StateDot({ node }: { node: TreeNode }) {
  const view = runStateView(node.state, node.waitingTasks);
  if (!view) return <AgentState state="idle" compact />;
  return <AgentState state={view.state} compact {...(view.label ? { label: view.label } : {})} />;
}

function AgentRow({ node, summary, size }: { node: TreeNode; summary: AgentSummary | undefined; size: number }) {
  const href = useAppHref();
  const identity = agentIdentity({ slug: node.slug, name: node.name, icon: node.icon });
  const caption = summary?.model ? modelLabel(summary.model_label || summary.model, summary.effort) : '';
  return (
    <ListRow
      lead={<AgentAvatar name={identity.name} role={identity.role} color={identity.color} size={size} />}
      title={node.name}
      meta={
        <>
          {node.roleTitle && node.roleTitle !== node.name && <span>{node.roleTitle}</span>}
          <StateDot node={node} />
          <ContextCount value={node.contextPct} />
        </>
      }
      trail={caption ? <span className="app-team-caption">{caption}</span> : undefined}
      href={href('/agents/' + encodeURIComponent(node.slug))}
    />
  );
}

interface GroupProps {
  label: string;
  lead: ReactNode;
  nodes: readonly TreeNode[];
  summaries: ReadonlyMap<string, AgentSummary>;
  size: number;
}

/** One reporting line: a header naming the manager, then that manager's reports in one raised container. */
function Group({ label, lead, nodes, summaries, size }: GroupProps) {
  return (
    <section className="app-team-group" aria-label={label}>
      <div className="app-team-head">
        {lead}
        <Text variant="label" tone="muted">
          {label}
        </Text>
      </div>
      <div className="app-team-list">
        <div className="app-team-rows">
          {nodes.map((n) => (
            <AgentRow key={n.slug} node={n} summary={summaries.get(n.slug)} size={size} />
          ))}
        </div>
      </div>
      {nodes
        .filter((n) => n.children.length > 0)
        .map((n) => {
          const identity = agentIdentity({ slug: n.slug, name: n.name, icon: n.icon });
          return (
            <div key={n.slug} className="app-team-branch">
              <Group
                label={'Reports to ' + n.name}
                lead={<AgentAvatar name={identity.name} role={identity.role} color={identity.color} size={20} />}
                nodes={n.children}
                summaries={summaries}
                size={size}
              />
            </div>
          );
        })}
    </section>
  );
}

/** The deepest indent step the stylesheet draws; deeper reports sit at this one. */
const MAX_INDENT = 5;

/**
 * Below 960, as the canvas draws the phone: one list of the reporting lines, the person at the root (not a
 * link) and each agent indented one step per level under its manager.
 */
function ReportingLines({ person, tree, summaries }: { person: string; tree: readonly TreeNode[]; summaries: ReadonlyMap<string, AgentSummary> }) {
  const rows: { node: TreeNode; level: number }[] = [];
  const walk = (nodes: readonly TreeNode[], level: number) => {
    for (const n of nodes) {
      rows.push({ node: n, level });
      walk(n.children, level + 1);
    }
  };
  walk(tree, 1);
  return (
    <section className="app-team-group" aria-label="Reporting lines">
      <div className="app-team-head">
        <Text variant="label" tone="muted">
          Reporting lines
        </Text>
      </div>
      <div className="app-team-list">
        <div className="app-team-rows">
          <ListRow lead={<PersonAvatar name={person} size={32} />} title={person} meta="You" />
          {rows.map(({ node, level }) => (
            <div key={node.slug} className={'app-team-indent app-team-indent-' + Math.min(level, MAX_INDENT)}>
              <AgentRow node={node} summary={summaries.get(node.slug)} size={32} />
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}

/** "6 agents · 3 working · 1 needs help": the parts that are zero are left out. */
function summaryLine(nodes: readonly TreeNode[]): string {
  const working = nodes.filter((n) => n.state === 'running').length;
  const help = nodes.filter((n) => n.state === 'needs_help' || n.state === 'quarantined').length;
  return [nodes.length + (nodes.length === 1 ? ' agent' : ' agents'), working ? working + ' working' : '', help ? help + ' needs help' : ''].filter(Boolean).join(' · ');
}

/** The team: the org by reporting line with each agent's live state, and the archive folded at the foot. */
export function Team() {
  useFrameChrome({ title: 'Team' });
  const { org, me } = useOrg();
  const deps = useChatDeps();
  const href = useAppHref();
  const wide = useMediaQuery(mediaQuery(BREAKPOINT_TABBAR));
  const { data, refresh } = useFetched<AgentsResponse>('/api/v1/agents');

  const nodes = useMemo(() => flattenTree(org.tree), [org.tree]);
  const membership = nodes.map((n) => n.slug).join(',');
  const seen = useRef<string | null>(null);
  useEffect(() => {
    // A hire or an offboard changes who is on the list; the summaries (model, archive) are read again. The
    // first snapshot is the baseline the mount's read already stands for.
    if (!org.ready) return;
    if (seen.current !== null && seen.current !== membership) void refresh();
    seen.current = membership;
  }, [org.ready, membership, refresh]);

  const summaries = useMemo(() => new Map((data?.agents ?? []).map((a) => [a.slug, a])), [data]);
  const archived = data?.archived ?? [];
  const nameOf = useMemo(() => new Map([...(data?.agents ?? []), ...(data?.archived ?? [])].map((a) => [a.slug, a.name])), [data]);
  const person = me?.org.owner_name || me?.user.name || 'You';
  const now = new Date(deps.now());

  return (
    <div className="app-team">
      <div className="app-team-title">
        <PageTitle>Team</PageTitle>
        {org.ready && nodes.length > 0 && (
          <Text as="p" variant="label" tone="muted">
            {summaryLine(nodes)}
          </Text>
        )}
      </div>
      {!org.ready ? (
        <div className="app-team-list" aria-busy="true">
          <Skeleton height={64} />
          <Skeleton height={64} />
        </div>
      ) : nodes.length === 0 ? (
        <div className="app-team-list">
          <EmptyState title="No agents yet">Every team starts with one.</EmptyState>
        </div>
      ) : wide ? (
        <Group label="Reports to you" lead={<PersonAvatar name={person} size={20} />} nodes={org.tree} summaries={summaries} size={40} />
      ) : (
        <ReportingLines person={person} tree={org.tree} summaries={summaries} />
      )}
      {archived.length > 0 && (
        <Card collapsible defaultOpen={false} title="Archived" meta={archived.length + (archived.length === 1 ? ' agent' : ' agents') + ' · their files are kept'}>
          <div className="app-team-archive">
            {archived.map((a) => {
              const identity = agentIdentity({ slug: a.slug, name: a.name, icon: a.icon });
              // "Offboarded 3 Sept · reported to Chief of Staff", as the canvas writes it.
              const when = a.archived_at ? 'Offboarded ' + shortDate(new Date(a.archived_at), now) : 'Offboarded';
              const manager = a.reports_to === 'ceo' ? 'you' : (nameOf.get(a.reports_to) ?? a.reports_to);
              return (
                <ListRow
                  key={a.slug}
                  lead={<AgentAvatar name={identity.name} role={identity.role} color={identity.color} size={32} />}
                  title={a.name}
                  meta={[when, manager ? 'reported to ' + manager : ''].filter(Boolean).join(' · ')}
                  trail={<Icon name="chevron-right" />}
                  href={href('/agents/' + encodeURIComponent(a.slug))}
                />
              );
            })}
          </div>
        </Card>
      )}
    </div>
  );
}
