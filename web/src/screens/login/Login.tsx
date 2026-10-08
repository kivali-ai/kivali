import { useEffect, useState } from 'react';
import { useSearchParams } from 'react-router';
import { apiGet } from '../../api/client';
import type { Login as LoginInfo } from '../../api/types.gen';
import { Bare } from '../../app/Bare';
import { ORG_LOGO_SRC } from '../../app/orgIdentity';
import { Banner, Button, OrgMark, Text } from '../../ds';
import { useDocumentTitle } from '../../lib/documentTitle';

export interface LoginProps {
  /** Starts sign-in. Defaults to navigating to the server's /auth/login. */
  onSignIn?: (next: string) => void;
  /** Continues without signing in (dev mode). Defaults to navigating to `next`. */
  onContinue?: (next: string) => void;
}

/** A same-site path to come back to, or "/". Blocks protocol-relative and absolute URLs. */
export function safeNext(next: string | null): string {
  if (!next || !next.startsWith('/') || next.startsWith('//') || next.startsWith('/\\')) return '/';
  return next;
}

function signInViaServer(next: string): void {
  window.location.assign('/auth/login?next=' + encodeURIComponent(next));
}

function continueTo(next: string): void {
  window.location.assign(next);
}

/**
 * Sign in, outside the frame (canvases 6d and 6e), fed by the public GET /api/v1/login: the org's mark and
 * name, whether Google sign-in is connected, and whether this machine bypasses sign-in. When sign-in is not
 * connected it says so and who can fix it, and nothing else.
 */
export function Login({ onSignIn = signInViaServer, onContinue = continueTo }: LoginProps) {
  useDocumentTitle('Sign in');
  const [params] = useSearchParams();
  const requested = params.get('next');
  const next = requested === null ? '/' : safeNext(requested);
  const [info, setInfo] = useState<LoginInfo | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    const abort = new AbortController();
    // Never a 401, but the option keeps a stray one from bouncing the browser back to this page.
    apiGet<LoginInfo>('/api/v1/login', { signal: abort.signal, redirectOn401: false })
      .then(setInfo)
      .catch((err: unknown) => {
        if (!(err instanceof DOMException && err.name === 'AbortError')) setFailed(true);
      });
    return () => abort.abort();
  }, []);

  const loading = info === null && !failed;
  const devMode = info?.dev_mode === true;
  const configured = info ? info.auth_ready : true;
  const orgName = info?.org.name ?? '';
  const owner = orgName ? orgName + '’s owner' : 'the team’s owner';
  const showNotReady = !configured && !devMode;

  return (
    <Bare width={showNotReady ? 'medium' : 'narrow'}>
      <div className="app-login">
        {orgName && (
          <div className="app-login-mark">
            <OrgMark name={orgName} size={40} {...(info?.org.has_logo ? { src: ORG_LOGO_SRC } : {})} />
          </div>
        )}
        <div className="app-login-head">
          <Text as="h1" variant="title">
            {orgName || 'Sign in'}
          </Text>
          {orgName && !showNotReady && !devMode && (
            <Text tone="muted" as="p">
              Sign in to continue.
            </Text>
          )}
        </div>
        {devMode ? (
          <Button variant="primary" onClick={() => onContinue(next)}>
            Continue
          </Button>
        ) : (
          <Button variant="primary" disabled={loading || !configured} onClick={() => onSignIn(next)}>
            Sign in with Google
          </Button>
        )}
        {showNotReady && (
          <Banner tone="warning" title="Sign-in isn’t set up yet">
            Whoever runs this Kivali server needs to connect Google sign-in. Once they have, this page lets you in.
          </Banner>
        )}
        {devMode ? (
          <Text as="p" variant="caption" tone="muted" className="app-login-note">
            Sign-in is bypassed on this machine.
          </Text>
        ) : (
          <Text as="p" variant="caption" tone="muted" className="app-login-note">
            Only {owner} can sign in.
          </Text>
        )}
      </div>
    </Bare>
  );
}
