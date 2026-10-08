import { useEffect, useRef } from 'react';
import { useNavigate } from 'react-router';
import { apiGet } from '../api/client';
import type { Setup } from '../api/types.gen';

/** Whether this page load has asked /api/v1/setup yet. One per route table, so one per load. */
export interface SetupGate {
  checked: boolean;
}

export function createSetupGate(): SetupGate {
  return { checked: false };
}

/**
 * Sends an org that is not set up yet from Home to the wizard, the way the server's "/" always has. It asks
 * once per load, the first time Home shows to someone signed in; a failed ask leaves Home where it is.
 */
export function useSetupRedirect(signedIn: boolean, onHome: boolean, gate: SetupGate | undefined): void {
  const navigate = useNavigate();
  // Someone who left Home while the answer was on its way stays where they went.
  const stillHome = useRef(onHome);
  stillHome.current = onHome;
  useEffect(() => {
    if (!gate || gate.checked || !signedIn || !onHome) return;
    gate.checked = true;
    apiGet<Setup>('/api/v1/setup')
      .then((setup) => {
        if (setup.needed && stillHome.current) void navigate('/setup', { replace: true });
      })
      .catch(() => {
        // Home is still a page: nothing to do.
      });
  }, [gate, signedIn, onHome, navigate]);
}
