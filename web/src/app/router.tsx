import type { ReactNode } from 'react';
import { createBrowserRouter, Outlet } from 'react-router';
import type { RouteObject } from 'react-router';
import type { EventStream } from '../api/sse';
import { Gallery } from '../screens/gallery/Gallery';
import { Agent } from '../screens/agent/Agent';
import { Graph } from '../screens/graph/Graph';
import { Home } from '../screens/home/Home';
import { Login } from '../screens/login/Login';
import { NotFound } from '../screens/notfound/NotFound';
import { Org } from '../screens/org/Org';
import { Proposal } from '../screens/proposals/Proposal';
import { Setup } from '../screens/setup/Setup';
import { Team } from '../screens/team/Team';
import { Assignment, Work } from '../screens/work/Work';
import { OrgProvider } from '../state/OrgProvider';
import { Frame } from './Frame';
import { useInternalLinkInterception } from './links';
import { RouteError } from './RouteError';
import { createSetupGate } from './setupRedirect';

/** Seams for tests: the org stream and what Sign out does. */
export interface RouteDeps {
  createStream?: () => EventStream;
  onSignOut?: () => void;
}

function Root() {
  useInternalLinkInterception();
  return <Outlet />;
}


// Each route gets its own boundary, so one broken screen leaves the frame and its neighbours standing.
function screen(path: string, element: ReactNode): RouteObject {
  return { path, element, errorElement: <RouteError /> };
}

/** The route table. The frame (and the org data behind it) wraps only the signed-in screens. */
export function createAppRoutes(deps: RouteDeps = {}): RouteObject[] {
  const provider = deps.createStream ? { createStream: deps.createStream } : {};
  const frame = deps.onSignOut ? { onSignOut: deps.onSignOut } : {};
  // One route table per load, so the setup check runs once per load.
  const setupGate = createSetupGate();
  return [
    {
      element: <Root />,
      errorElement: <RouteError />,
      children: [
        screen('login', <Login />),
        screen('_ds', <Gallery />),
        screen('setup/*', <Setup />),
        {
          element: (
            <OrgProvider {...provider}>
              <Frame {...frame} setupGate={setupGate} />
            </OrgProvider>
          ),
          errorElement: <RouteError />,
          children: [
            { index: true, element: <Home />, errorElement: <RouteError /> },
            screen('team', <Team />),
            screen('agents/:slug', <Agent section="chat" />),
            screen('agents/:slug/background', <Agent section="background" />),
            screen('agents/:slug/about', <Agent section="about" />),
            screen('agents/:slug/chats', <Agent section="chats" />),
            screen('agents/:slug/chats/:ts', <Agent section="past-chat" />),
            screen('agents/:slug/subagents/:id', <Agent section="subagent" />),
            screen('proposals/*', <Proposal />),
            screen('work', <Work />),
            screen('assignments/:id', <Assignment />),
            screen('graph', <Graph />),
            screen('org', <Org />),
            screen('org/:section', <Org />),
          ],
        },
        screen('*', <NotFound />),
      ],
    },
  ];
}

// The app is mounted at the site root; the server answers its own paths (see links.ts).
export function createAppRouter() {
  return createBrowserRouter(createAppRoutes(), { basename: '/' });
}
