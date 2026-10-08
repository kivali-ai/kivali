import { useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { ApiError } from '../../api/client';
import type { PersonRef } from '../../api/types.gen';
import { runStateView } from '../../app/runState';
import { AgentAvatar, AgentState, PersonAvatar } from '../../ds';
import type { AssignmentAssignee, AssignmentReadiness } from '../../ds';
import { agentIdentity } from '../../lib/agentIdentity';
import { flattenTree } from '../../state/org';
import type { TreeNode } from '../../state/org';
import { useOrg, usePersonName } from '../../state/OrgProvider';

/** The slug the server gives the person in PersonRef. */
export const YOU = 'ceo';

/** Skeletons appear only when loading takes longer than this; faster loads just appear. */
export const SKELETON_DELAY_MS = 300;

export function asApiError(err: unknown): ApiError {
  return err instanceof ApiError ? err : new ApiError(0, 'Something went wrong in Kivali.', 'Reload the page. If it keeps happening, whoever runs this Kivali server can look into it.');
}

/**
 * The design system's assignment state for a tracker state. Moving reads as ready (a circle that can move now);
 * a closed assignment is done, or dropped when that is how it ended.
 */
export function readiness(state: string, resolution?: string): AssignmentReadiness {
  switch (state) {
    case 'closed':
      return resolution === 'dropped' ? 'dropped' : 'done';
    case 'on_hold':
      return 'held';
    case 'blocked':
      return 'blocked';
    default:
      return 'ready';
  }
}

/** The word AssignmentState shows: the design system says "Ready" for both ready and moving, so moving overrides it. */
export function stateLabel(state: string): string | undefined {
  return state === 'moving' ? 'Moving' : undefined;
}

/** True once `loading` has lasted SKELETON_DELAY_MS. */
export function useSlow(loading: boolean): boolean {
  const [slow, setSlow] = useState(false);
  useEffect(() => {
    if (!loading) return;
    const t = setTimeout(() => setSlow(true), SKELETON_DELAY_MS);
    return () => clearTimeout(t);
  }, [loading]);
  return slow && loading;
}

export interface Faces {
  /** An agent's tile, or your circle. */
  face(who: PersonRef, size: 20 | 24): ReactNode;
  /** The row assignee prop AssignmentRow takes. */
  assignee(who: PersonRef): AssignmentAssignee;
  /** The team-tree node for an agent, when the org snapshot lists it. */
  node(slug: string): TreeNode | undefined;
}

/** Faces drawn from the team tree so an agent looks here as it does in the sidebar. */
export function useFaces(): Faces {
  const { org } = useOrg();
  const personName = usePersonName();
  return useMemo(() => {
    const nodes = new Map(flattenTree(org.tree).map((n) => [n.slug, n]));
    const identity = (who: PersonRef) => agentIdentity({ slug: who.slug, name: who.name || nodes.get(who.slug)?.name || who.slug, icon: nodes.get(who.slug)?.icon });
    return {
      face: (who, size) => (who.slug === YOU ? <PersonAvatar name={personName} size={size} /> : <AgentAvatar {...identity(who)} size={size} />),
      assignee: (who) => (who.slug === YOU ? { kind: 'person', name: who.name } : { kind: 'agent', ...identity(who) }),
      node: (slug) => nodes.get(slug),
    };
  }, [org.tree, personName]);
}

/** A compact run state for an agent that is doing something; nothing for one at rest or one the tree does not list. */
export function CompactState({ node }: { node: TreeNode | undefined }) {
  const view = node ? runStateView(node.state, node.waitingTasks) : null;
  if (!view) return null;
  return <AgentState state={view.state} compact {...(view.label ? { label: view.label } : {})} />;
}
