import type { AgentState, AutoRelease, Goal, OrgSnapshot, Readouts } from '../api/types.gen';

/** One agent in the team tree. Children follow the reporting line; `depth` is the server's (1 for the person's direct reports). */
export interface TreeNode {
  slug: string;
  name: string;
  roleTitle: string;
  /** The role icon its avatar shows; empty for initials only. */
  icon: string;
  reportsTo: string;
  depth: number;
  state: AgentState;
  contextPct: number;
  /** Background tasks still running for this agent; 0 when the snapshot omits the count. */
  waitingTasks: number;
  children: TreeNode[];
}

export interface OrgState {
  /** False until the first snapshot has been applied. */
  ready: boolean;
  /** The person's direct reports; the person is the root and is drawn by the frame. */
  tree: TreeNode[];
  /** Items waiting on the person: inbox.unactioned. */
  needs: number;
  /** True while the release engine runs or any agent is running a turn. */
  running: boolean;
  autoRelease: AutoRelease;
  pendingPaths: string[];
  ceoPaths: string[];
  messagesTotal: number;
  assignmentsVersion: number;
  goals: Goal[];
  readouts: Readouts;
}

export const initialOrgState: OrgState = {
  ready: false,
  tree: [],
  needs: 0,
  running: false,
  autoRelease: 'off',
  pendingPaths: [],
  ceoPaths: [],
  messagesTotal: 0,
  assignmentsVersion: 0,
  goals: [],
  readouts: { working: 0, blocked: 0, closed_week: 0, spend_today: 0, spend_7d: 0 },
};

/** The slug the server gives the person's direct reports as `reports_to`. */
const ROOT = 'ceo';

/** Builds the nested tree. The payload is already in tree order, so siblings keep the order they arrive in. */
export function buildTree(agents: OrgSnapshot['agents']): TreeNode[] {
  const nodes = new Map<string, TreeNode>();
  for (const a of agents) {
    nodes.set(a.slug, {
      slug: a.slug,
      name: a.name || a.slug,
      roleTitle: a.role_title,
      icon: a.icon,
      reportsTo: a.reports_to,
      depth: a.depth,
      state: a.state,
      contextPct: a.context_pct,
      waitingTasks: a.waiting_tasks ?? 0,
      children: [],
    });
  }
  const roots: TreeNode[] = [];
  for (const a of agents) {
    const node = nodes.get(a.slug);
    if (!node) continue;
    const parent = a.reports_to === ROOT ? undefined : nodes.get(a.reports_to);
    // A manager that is not in the list (archived, or a stale payload) leaves its reports at the top rather than dropping them.
    if (parent && parent !== node) parent.children.push(node);
    else roots.push(node);
  }
  return roots;
}

/** Depth-first, parents before children: the order the sidebar draws rows in. */
export function flattenTree(tree: TreeNode[]): TreeNode[] {
  const out: TreeNode[] = [];
  const walk = (nodes: TreeNode[]) => {
    for (const n of nodes) {
      out.push(n);
      walk(n.children);
    }
  };
  walk(tree);
  return out;
}

/** Pure: the org state after a snapshot. `prev` is kept for the fields a snapshot never carries. */
export function reduceSnapshot(prev: OrgState, snapshot: OrgSnapshot): OrgState {
  return {
    ...prev,
    ready: true,
    tree: buildTree(snapshot.agents),
    needs: snapshot.inbox.unactioned,
    running: snapshot.release.running || snapshot.agents.some((a) => a.state === 'running'),
    autoRelease: snapshot.inbox.auto_release,
    pendingPaths: snapshot.inbox.pending_paths,
    ceoPaths: snapshot.inbox.ceo_paths,
    messagesTotal: snapshot.messages_total,
    assignmentsVersion: snapshot.assignments_version,
    goals: snapshot.goals,
    readouts: snapshot.readouts,
  };
}

/** Local change after the person moves the slider, before the server confirms. */
export function withAutoRelease(prev: OrgState, value: AutoRelease): OrgState {
  return prev.autoRelease === value ? prev : { ...prev, autoRelease: value };
}

// The AutoRelease component names its stops "Now" and "Off"; the API uses lowercase.
const STOP_LABELS: Record<AutoRelease, string> = {
  now: 'Now',
  '30s': '30s',
  '2m': '2m',
  '5m': '5m',
  '20m': '20m',
  off: 'Off',
};

export function autoReleaseLabel(value: AutoRelease): string {
  return STOP_LABELS[value];
}

/** The wire value for a stop label, or undefined for a label that is not one of the six stops. */
export function autoReleaseValue(label: string): AutoRelease | undefined {
  const lower = label.toLowerCase();
  return (Object.keys(STOP_LABELS) as AutoRelease[]).find((k) => k === lower);
}
