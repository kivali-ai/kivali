/** The five destinations, in order. Icons are names from src/ds/Icon/icons.ts. */
export type DestinationKey = 'home' | 'team' | 'work' | 'graph' | 'org';

export interface Destination {
  key: DestinationKey;
  label: string;
  icon: string;
  to: string;
}

export const DESTINATIONS: readonly Destination[] = [
  { key: 'home', label: 'Home', icon: 'inbox', to: '/' },
  { key: 'team', label: 'Team', icon: 'users', to: '/team' },
  { key: 'work', label: 'Work', icon: 'list-todo', to: '/work' },
  { key: 'graph', label: 'Graph', icon: 'network', to: '/graph' },
  { key: 'org', label: 'Org', icon: 'settings', to: '/org' },
];

function within(pathname: string, base: string): boolean {
  return pathname === base || pathname.startsWith(base + '/');
}

/**
 * Which destination a path belongs to. Sub-pages keep their parent active: proposals sit under Home,
 * an agent's pages under Team, an assignment under Work.
 */
export function activeDestination(pathname: string): DestinationKey | null {
  if (pathname === '/' || within(pathname, '/proposals')) return 'home';
  if (within(pathname, '/team') || within(pathname, '/agents')) return 'team';
  if (within(pathname, '/work') || within(pathname, '/assignments')) return 'work';
  if (within(pathname, '/graph')) return 'graph';
  if (within(pathname, '/org')) return 'org';
  return null;
}

/** The slug of the agent whose pages are open, or null. */
export function activeAgentSlug(pathname: string): string | null {
  const m = /^\/agents\/([^/]+)/.exec(pathname);
  if (!m?.[1]) return null;
  try {
    return decodeURIComponent(m[1]);
  } catch {
    return m[1];
  }
}
