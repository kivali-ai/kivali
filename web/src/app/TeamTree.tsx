import type { ReactNode } from 'react';
import { AgentAvatar, AgentState, ContextCount, PersonAvatar, Text } from '../ds';
import { agentIdentity } from '../lib/agentIdentity';
import { flattenTree } from '../state/org';
import type { TreeNode } from '../state/org';
import { NameSep, NavLinkItem } from './NavLinkItem';
import { runStateView } from './runState';

/**
 * Context fill shows beside an agent in the tree from this percentage on: ContextCount's own default, 80
 * (the same point where the chat header's gauge turns into the honey badge).
 */
export const TREE_CONTEXT_THRESHOLD = 80;

/** The run-state dots and the context percentage, or null (NavItem draws nothing for a null count). */
function agentCount(node: TreeNode): ReactNode {
  const view = runStateView(node.state, node.waitingTasks);
  // ContextCount rounds before comparing; match it so the wrapper never renders empty.
  const showContext = Math.round(node.contextPct) >= TREE_CONTEXT_THRESHOLD;
  if (!view && !showContext) return null;
  return (
    <span className="app-tree-count">
      {view && (
        <>
          <NameSep />
          <AgentState state={view.state} compact {...(view.label ? { label: view.label } : {})} />
        </>
      )}
      {showContext && (
        <>
          <NameSep />
          <ContextCount value={node.contextPct} threshold={TREE_CONTEXT_THRESHOLD} />
        </>
      )}
    </span>
  );
}

export interface TeamTreeProps {
  tree: TreeNode[];
  person: string;
  activeSlug: string | null;
}

/** The person as the root (not a link), then every agent by reporting line. */
export function TeamTree({ tree, person, activeSlug }: TeamTreeProps) {
  return (
    <div className="app-tree" role="group" aria-label="Team">
      <div className="app-tree-person">
        <PersonAvatar name={person} size={20} />
        <span className="app-tree-person-name">{person}</span>
        <Text variant="label" tone="muted">
          you
        </Text>
      </div>
      {flattenTree(tree).map((n) => (
        <NavLinkItem
          key={n.slug}
          to={'/agents/' + encodeURIComponent(n.slug)}
          // The label already names the agent; without this the link's name says it twice.
          lead={
            <span className="app-nav-lead" aria-hidden="true">
              <AgentAvatar {...agentIdentity(n)} size={20} />
            </span>
          }
          label={n.name}
          depth={n.depth}
          active={n.slug === activeSlug}
          count={agentCount(n)}
        />
      ))}
    </div>
  );
}
