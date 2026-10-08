// An agent's face everywhere it is drawn: the role icon its hiring agent picked (the agent record's `icon`) and an
// identity colour from its slug. The same agent gets the same icon and colour on Home, in the sidebar tree, on the
// Team page, in its chat and on its page header. Identity colours mean who, never status. Draw an agent through
// agentIdentity (or an Identify built from the team tree), never with a bare name: AgentAvatar's own fallback
// hashes the name, which gives the same agent a different colour.
import { ROLE_ICONS, identityColors } from '../ds';
import { flattenTree } from '../state/org';
import type { TreeNode } from '../state/org';

/** A stable identity colour for an agent, hashed from its slug (which never changes, unlike its name). */
export function identityColorFor(slug: string): string {
  let h = 0;
  for (let i = 0; i < slug.length; i++) h = (h * 31 + slug.charCodeAt(i)) >>> 0;
  return identityColors[h % identityColors.length] ?? 'slate';
}

/** What AgentAvatar (and GoalRow, QueueRow) take for one agent. */
export interface AgentIdentity {
  name: string;
  /** The role icon; absent when the agent has none the avatar can draw, so it shows its initials only. */
  role?: string;
  color: string;
}

export function agentIdentity(agent: { slug: string; name: string; icon?: string | undefined }): AgentIdentity {
  const identity: AgentIdentity = { name: agent.name || agent.slug, color: identityColorFor(agent.slug) };
  if (agent.icon && agent.icon in ROLE_ICONS) identity.role = agent.icon;
  return identity;
}

/** The Chief of Staff as setup draws it before it is hired: the face it keeps once it is on the team. */
export const CHIEF_OF_STAFF: AgentIdentity = agentIdentity({ slug: 'chief-of-staff', name: 'Chief of Staff', icon: 'compass' });

export type Identify = (who: { slug: string; name?: string | undefined }) => AgentIdentity;

/**
 * An agent's role icon and identity colour from the team tree, so every screen draws the same face as the
 * sidebar. An agent the tree does not list (archived, or before the first snapshot) keeps its slug's colour and
 * shows its initials; its name comes from the caller.
 */
export function identifierFor(tree: TreeNode[]): Identify {
  const nodes = new Map(flattenTree(tree).map((n) => [n.slug, n]));
  return (who) => {
    const node = nodes.get(who.slug);
    return agentIdentity({ slug: who.slug, name: who.name || node?.name || who.slug, icon: node?.icon });
  };
}
