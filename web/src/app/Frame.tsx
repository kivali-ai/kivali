import { useCallback, useEffect, useMemo, useState } from 'react';
import { Outlet, useLocation, useNavigate } from 'react-router';
import { Banner, Button, Icon } from '../ds';
import { cx } from '../lib/cx';
import { documentTitle } from '../lib/documentTitle';
import { useOrg } from '../state/OrgProvider';
import { FrameChromeContext } from './chrome';
import type { FrameChrome, FrameChromeControl } from './chrome';
import { DESTINATIONS, activeAgentSlug, activeDestination } from './nav';
import { PhoneHeader } from './PhoneHeader';
import { useSetupRedirect } from './setupRedirect';
import type { SetupGate } from './setupRedirect';
import { Sidebar } from './Sidebar';
import { TabBar } from './TabBar';

export interface FrameProps {
  /** Ends the session. Defaults to navigating to /auth/logout. */
  onSignOut?: () => void;
  /** Asks once per load whether the org still needs setup, and sends Home to the wizard if so. */
  setupGate?: SetupGate;
}

function signOutViaServer(): void {
  window.location.assign('/auth/logout');
}

/**
 * The app frame: sidebar (desktop) or phone header and tab bar (below 960), and the content column that
 * renders the matched route. Screens adjust it through `useFrameChrome`.
 */
export function Frame({ onSignOut = signOutViaServer, setupGate }: FrameProps) {
  const { pathname } = useLocation();
  const navigate = useNavigate();
  const { me, org, loadError, actionError, dismissActionError } = useOrg();
  const [chrome, setChromeState] = useState<FrameChrome>({});

  const setChrome = useCallback((next: FrameChrome | null) => setChromeState(next ?? {}), []);
  const control = useMemo<FrameChromeControl>(() => ({ setChrome }), [setChrome]);

  const active = activeDestination(pathname);
  const activeAgent = activeAgentSlug(pathname);
  const destinationLabel = DESTINATIONS.find((d) => d.key === active)?.label;
  const title = chrome.title ?? destinationLabel ?? '';
  const isHome = pathname === '/';
  const tabBar = chrome.tabBar !== false;
  const width = chrome.width ?? 'content';

  useSetupRedirect(me !== null, isHome, setupGate);

  useEffect(() => {
    document.title = documentTitle(isHome ? undefined : title || undefined);
  }, [isHome, title]);

  const back = chrome.back;

  return (
    <FrameChromeContext.Provider value={control}>
      <div className="app-frame">
        <Sidebar active={active} activeAgent={activeAgent} onSignOut={onSignOut} />
        <div className="app-main">
          <PhoneHeader title={title} back={back} />
          <main className="app-page">
            <div className={cx('app-column', 'is-' + width)}>
              {back && (
                <div className="app-back">
                  <Button variant="ghost" size="sm" icon={<Icon name="chevron-left" />} onClick={() => void navigate(back.to)}>
                    {back.label}
                  </Button>
                </div>
              )}
              {loadError && (
                <Banner tone="danger" title={loadError.message}>
                  {loadError.who}
                </Banner>
              )}
              {actionError && (
                <Banner tone="danger" title={actionError.message} onDismiss={dismissActionError}>
                  {actionError.who}
                </Banner>
              )}
              <Outlet />
            </div>
          </main>
          {tabBar && <TabBar active={active} needs={org.needs} />}
        </div>
      </div>
    </FrameChromeContext.Provider>
  );
}
