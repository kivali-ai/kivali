import { useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { useHref, useLocation, useNavigate, useParams } from 'react-router';
import { apiDelete, apiGet, apiPost, apiPut } from '../../api/client';
import type { ApiError } from '../../api/client';
import type {
  AccessRow,
  BrowserSignIn,
  ConnectionField,
  ConnectionState,
  ConnectionWrite,
  DeviceSignIn,
  IntegrationAudit,
  IntegrationAuditRow,
  IntegrationLogLine,
  IntegrationLogs,
  IntegrationPage as IntegrationPageData,
  IntegrationVersion,
  IntegrationVersionSummary,
  PersonRef,
  SignInProgress,
} from '../../api/types.gen';
import { useFrameChrome } from '../../app/chrome';
import { PageTitle } from '../../app/PageTitle';
import { Badge, Banner, Button, Card, Dialog, DialogClose, Icon, ListRow, Select, Skeleton, Switch, Table, Text, TextField } from '../../ds';
import type { BadgeTone, TableColumn } from '../../ds';
import { AccessDialog } from './AccessDialog';
import {
  Block,
  Markdown,
  RollBackDialog,
  signInErrorWords,
  ScopesLine,
  StatusBadge,
  VersionBadge,
  actionWords,
  agentsLabel,
  durationLabel,
  integrationApi,
  invalidAccessLine,
  isRollBack,
  rollBackApi,
  versionPath,
  whenLabel,
} from './integrationParts';
import { ErrorBanner, None, RowList } from './parts';
import { asApiError, useAction, useResource } from './useResource';
import '../../styles/org.css';
import '../../styles/integrations.css';

/** The field values a connection form starts with: what is stored for plain fields; secrets start blank. */
function startingValues(fields: readonly ConnectionField[]): Record<string, string> {
  return Object.fromEntries(fields.map((f) => [f.key, f.sensitive ? '' : (f.value ?? '')]));
}

/** What Save connection sends: every plain field as typed, and only the secrets typed into (blank keeps one). */
export function connectionBody(fields: readonly ConnectionField[], values: Record<string, string>): ConnectionWrite {
  const out: Record<string, string> = {};
  for (const f of fields) {
    const v = (values[f.key] ?? '').trim();
    if (f.sensitive && v === '') continue;
    out[f.key] = v;
  }
  return { fields: out };
}

/** What Test connection last found, when it worked. A failed test shows as a banner instead (see ConnectionForm). */
function verifiedLine(c: ConnectionState, now: number): string | undefined {
  if (!c.verified_at || !c.verified) return undefined;
  const said = c.verify_message ?? '';
  return 'Verified ' + whenLabel(c.verified_at, now) + (said ? ': ' + said : '');
}

/** The lines under the connection form: since when it is connected and what Test connection last found. */
function connectionLines(c: ConnectionState | undefined, now: number): string[] {
  if (!c) return ['Not connected.'];
  const verified = verifiedLine(c, now);
  return ['Connected since ' + whenLabel(c.created_at, now), ...(verified ? [verified] : [])];
}

/**
 * Where a connection made by signing in stands: when the owner signed in and when its token is next refreshed, or
 * why they must sign in again. Undefined for a token connection.
 */
export function signInLine(c: ConnectionState | undefined, now: number): { text: string; danger: boolean } | undefined {
  const s = c?.sign_in;
  if (!s) return undefined;
  if (s.signed_in) {
    const at = s.signed_in_at ? ' ' + whenLabel(s.signed_in_at, now) : '';
    const refresh = s.expires_at ? ' · token refreshes ' + whenLabel(s.expires_at, now) : '';
    return { text: 'Signed in' + at + refresh, danger: false };
  }
  if (s.error) return { text: 'Needs you to sign in again: ' + s.error, danger: true };
  return { text: 'Not signed in yet.', danger: false };
}

/** The sign-in line of a connection made by signing in; nothing for a token connection. */
function SignInLine({ connection, now }: { connection: ConnectionState | undefined; now: number }) {
  const line = signInLine(connection, now);
  if (!line) return null;
  return (
    <Text as="p" variant="caption" tone={line.danger ? 'danger' : 'muted'}>
      {line.text}
    </Text>
  );
}

/** How often the page asks how a sign-in under way stands, while the tab is visible. */
export const SIGN_IN_POLL_MS = 3000;

/** How long a sign-in's code works, from its expiry: "The code works for 10 minutes." */
export function codeLifeWords(expiresAt: string, now: number): string {
  const minutes = Math.max(1, Math.round((Date.parse(expiresAt) - now) / 60_000));
  return 'The code works for ' + minutes + (minutes === 1 ? ' minute.' : ' minutes.');
}

/** Whether a sign-in connection's form holds both the client id and the client secret, which a sign-in needs. */
export function clientSaved(fields: readonly ConnectionField[]): boolean {
  const set = (key: string) => fields.some((f) => f.key === key && f.set);
  return set('client_id') && set('client_secret');
}

/**
 * Where the page sends the browser to begin the browser sign-in `flow` (BrowserSignIn.flow): Kivali sends it on to
 * the service's sign-in page. Kivali Desktop does not load this address in a team's window; it opens the system
 * browser at it instead, signed in to Kivali through the handoff (desktop-app/src-tauri/src/windows.rs, connect_flow).
 */
export function connectStartPath(flow: string): string {
  return '/connect/start?flow=' + encodeURIComponent(flow);
}

/**
 * Sends the browser on its way to the service's sign-in page for the browser sign-in `flow`. In a browser the page
 * leaves; in Kivali Desktop it stays, and the system browser opens instead.
 */
export function openSignIn(flow: string): void {
  window.location.assign(connectStartPath(flow));
}

/**
 * How a browser sign-in came back to the page: the connection it signed in, the connection whose sign-in failed
 * (its words are the connection's GET …/sign-in), or that there is no sign-in to read and the owner starts again.
 */
export type SignInReturn = { kind: 'signed_in'; id: string } | { kind: 'failed'; id: string } | { kind: 'start_again'; words: string };

/**
 * The page's query after a browser sign-in: ?signed_in=<id>, ?sign_in_failed=<id> or ?sign_in_error=<code>. The query
 * never carries words, since anyone can link to the page with any query: a code is one of signInErrorWords' and any
 * other is ignored.
 */
export function signInReturnOf(search: string): SignInReturn | null {
  const q = new URLSearchParams(search);
  const signedIn = q.get('signed_in');
  if (signedIn) return { kind: 'signed_in', id: signedIn };
  const failed = q.get('sign_in_failed');
  if (failed) return { kind: 'failed', id: failed };
  const words = signInErrorWords(q.get('sign_in_error'));
  return words ? { kind: 'start_again', words } : null;
}

/**
 * "Signs in at app.hubspot.com": the hosts the connection's ways send the owner to, each once, in the ways' order;
 * undefined when none is known.
 */
export function signsInAt(ways: readonly string[], at: Readonly<Record<string, string>> | undefined): string | undefined {
  const hosts: string[] = [];
  for (const way of ways) {
    const host = at?.[way];
    if (host && !hosts.includes(host)) hosts.push(host);
  }
  return hosts.length > 0 ? 'Signs in at ' + namesInWords(hosts) : undefined;
}

/** Whether the tab is showing; a hidden one asks the server nothing. */
function useVisible(): boolean {
  const [visible, setVisible] = useState(() => document.visibilityState !== 'hidden');
  useEffect(() => {
    const onVisibility = () => setVisible(document.visibilityState !== 'hidden');
    document.addEventListener('visibilitychange', onVisibility);
    return () => document.removeEventListener('visibilitychange', onVisibility);
  }, []);
  return visible;
}

/** What the Sign in block of one connection is wired to: its routes, and what to do with the page they answer. */
type SignInWiring = Pick<SignInBlockProps, 'api' | 'scopes' | 'signInAt' | 'onPage' | 'onSignedIn' | 'returned'>;

interface SignInBlockProps {
  /** The connection; absent before the client id and secret are first saved. */
  connection: ConnectionState | undefined;
  /** Its form, with what it holds. */
  fields: readonly ConnectionField[];
  /** The connection's routes: …/connection, or …/connections/{id}. */
  api: string;
  /** What the sign-in asks the service for, shown above the block. */
  scopes: readonly string[];
  /** The host each way's sign-in goes to (sign_in_at), named in the block so the owner sees where they are sent. */
  signInAt?: Readonly<Record<string, string>>;
  busy: boolean;
  now: number;
  /** The page as Sign out answers it. */
  onPage(page: IntegrationPageData): void;
  /** Reads the page again once a sign-in is done; settles once the page is in. */
  onSignedIn(): Promise<unknown>;
  /** How a browser sign-in of this connection came back to the page, when it just did. */
  returned?: SignInReturn | null;
}

/**
 * A connection made by signing in, once the client id and secret are saved: Sign in with a code, Sign in in your
 * browser, or both, the manifest's way first. With a code, the code and the service's page to enter it on while Kivali
 * waits for the approval, asking how it stands every few seconds while the tab is visible, and "Finishing the
 * sign-in." once it is done until the page is read again. In the browser, the page leaves for the service's
 * (openSignIn), which sends the browser back here through Kivali with how it went (returned); where the page stays
 * (Kivali Desktop, which opens the system browser instead), it waits for the sign-in as it does for a code, with
 * Start again. Signed in, who the
 * connection test found and Sign out; a sign-in that failed or expired, why, and Try again the same way; a connection
 * none of whose ways this version offers, a sentence saying so. What the sign-in asks the service for is above it.
 */
function SignInBlock({ connection, fields, api, scopes, signInAt, busy, now, onPage, onSignedIn, returned }: SignInBlockProps) {
  const [device, setDevice] = useState<DeviceSignIn | null>(null);
  // A browser sign-in under way that this page stayed for: Kivali Desktop opened the system browser.
  const [inBrowser, setInBrowser] = useState(false);
  // Done, and the page is being read again: held so the block does not show Sign in with a code in between.
  const [finishing, setFinishing] = useState(false);
  const [ended, setEnded] = useState<SignInProgress | null>(() =>
    returned?.kind === 'start_again' ? { state: 'expired', error: returned.words } : null,
  );
  // The way the sign-in that ended went, which Try again takes again.
  const [endedWay, setEndedWay] = useState<'device' | 'browser'>(returned && returned.kind !== 'signed_in' ? 'browser' : 'device');
  const [error, setError] = useState<ApiError | null>(null);
  const [working, setWorking] = useState(false);
  const [copied, setCopied] = useState(false);
  const visible = useVisible();
  const signedIn = useRef(onSignedIn);
  useEffect(() => {
    signedIn.current = onSignedIn;
  });
  const s = connection?.sign_in;

  // A browser sign-in that came back failed: why is the server's, read once from the sign-in it kept.
  const failedRead = useRef(false);
  useEffect(() => {
    if (returned?.kind !== 'failed' || failedRead.current) return;
    failedRead.current = true;
    apiGet<SignInProgress>(api + '/sign-in').then(
      (p) => {
        if (p.state === 'failed' || p.state === 'expired') setEnded(p);
      },
      (err: unknown) => setError(asApiError(err)),
    );
  }, [returned, api]);

  useEffect(() => {
    if ((!device && !inBrowser) || !visible) return;
    const way = device ? 'device' : 'browser';
    let stopped = false;
    const timer = setInterval(() => {
      apiGet<SignInProgress>(api + '/sign-in').then(
        (p) => {
          if (stopped || p.state === 'pending') return;
          setDevice(null);
          setInBrowser(false);
          if (p.state !== 'done') {
            setEndedWay(way);
            setEnded(p);
            return;
          }
          setFinishing(true);
          void signedIn.current().finally(() => setFinishing(false));
        },
        (err: unknown) => {
          if (!stopped) setError(asApiError(err));
        },
      );
    }, SIGN_IN_POLL_MS);
    return () => {
      stopped = true;
      clearInterval(timer);
    };
  }, [device, inBrowser, visible, api]);

  const start = async () => {
    setWorking(true);
    setError(null);
    setEnded(null);
    setCopied(false);
    try {
      setDevice(await apiPost<DeviceSignIn>(api + '/sign-in/device'));
    } catch (err) {
      setError(asApiError(err));
    } finally {
      setWorking(false);
    }
  };
  const startBrowser = async () => {
    setWorking(true);
    setError(null);
    setEnded(null);
    setInBrowser(false);
    try {
      const { flow } = await apiPost<BrowserSignIn>(api + '/sign-in/browser');
      openSignIn(flow);
      // In a browser the tab is leaving; in Kivali Desktop it stays and waits, with its buttons free again (finally),
      // Start again among them. A click in the moment before a leaving tab goes only starts the sign-in afresh.
      setInBrowser(true);
    } catch (err) {
      setError(asApiError(err));
    } finally {
      setWorking(false);
    }
  };
  const signOut = async () => {
    setWorking(true);
    setError(null);
    try {
      onPage(await apiPost<IntegrationPageData>(api + '/sign-out'));
    } catch (err) {
      setError(asApiError(err));
    } finally {
      setWorking(false);
    }
  };
  const copy = () => {
    if (!device) return;
    void navigator.clipboard?.writeText(device.user_code).then(
      () => setCopied(true),
      () => setCopied(false),
    );
  };

  const disabled = busy || working;
  const ways = s?.ways ?? ['device'];
  const wayButton = (way: string, i: number) =>
    way === 'browser' ? (
      <Button key={way} type="button" variant={i === 0 ? 'primary' : 'secondary'} disabled={disabled} onClick={() => void startBrowser()}>
        Sign in in your browser
      </Button>
    ) : (
      <Button key={way} type="button" variant={i === 0 ? 'primary' : 'secondary'} disabled={disabled} onClick={() => void start()}>
        Sign in with a code
      </Button>
    );
  let body: ReactNode;
  if (s?.signed_in) {
    const who = connection?.verified && connection.verify_message;
    body = (
      <>
        {returned?.kind === 'signed_in' && (
          <Text as="p" variant="caption" tone="muted">
            You signed in in your browser.
          </Text>
        )}
        <Text as="p" variant="body">
          {who ? 'Signed in. ' + who : 'Signed in.'}
        </Text>
        <div className="app-org-actions">
          <Button type="button" variant="secondary" disabled={disabled} onClick={() => void signOut()}>
            Sign out
          </Button>
        </div>
      </>
    );
  } else if (finishing) {
    body = (
      <Text as="p" variant="body" tone="muted">
        Finishing the sign-in.
      </Text>
    );
  } else if (ways.length === 0) {
    // The ways offered are this server's; a manifest may name only ways it does not have.
    body = (
      <Text as="p" variant="body" tone="muted">
        This version of Kivali has no way to sign in to this service.
      </Text>
    );
  } else if (device) {
    const href = device.verification_url_complete ?? device.verification_url;
    body = (
      <div className="app-int-device">
        <Text as="p" variant="body">
          Enter this code on the service’s page, then approve the sign-in there. Kivali finishes it here once you do.
        </Text>
        <div className="app-int-device-code">
          <Text variant="display" className="app-int-code" aria-label="Sign-in code">
            {device.user_code}
          </Text>
          <Button type="button" size="sm" variant="ghost" icon={<Icon name={copied ? 'check' : 'copy'} />} onClick={copy}>
            {copied ? 'Copied' : 'Copy code'}
          </Button>
        </div>
        <Text as="p" variant="body">
          {/* eslint-disable-next-line react/forbid-elements -- external */}
          <a href={href} target="_blank" rel="noopener">
            {device.verification_url}
          </a>
        </Text>
        <Text as="p" variant="caption" tone="muted">
          {codeLifeWords(device.expires_at, now) + ' Waiting for your approval.'}
        </Text>
      </div>
    );
  } else if (inBrowser) {
    body = (
      <>
        <Text as="p" variant="body">
          Approve the sign-in on the service’s page in your browser. Kivali finishes it here once you do.
        </Text>
        <Text as="p" variant="caption" tone="muted">
          Waiting for your approval.
        </Text>
        {/* Kivali Desktop opens no browser for a team elsewhere (it could not finish there): this says what to do. */}
        <Text as="p" variant="caption" tone="muted">
          If no browser opened, open the team in your browser and sign in from there.
        </Text>
        <div className="app-org-actions">
          <Button type="button" variant="secondary" disabled={disabled} onClick={() => void startBrowser()}>
            Start again
          </Button>
        </div>
      </>
    );
  } else if (ended) {
    body = (
      <>
        <Banner tone="danger" title={ended.state === 'expired' ? 'The sign-in expired' : 'The sign-in did not work'}>
          {ended.error ?? 'The service did not say why.'}
        </Banner>
        <div className="app-org-actions">
          <Button type="button" variant="primary" disabled={disabled} onClick={() => void (endedWay === 'browser' ? startBrowser() : start())}>
            Try again
          </Button>
        </div>
      </>
    );
  } else if (connection && clientSaved(fields)) {
    body = <div className="app-org-actions">{ways.map(wayButton)}</div>;
  } else {
    body = (
      <Text as="p" variant="body" tone="muted">
        Save the client id and secret first.
      </Text>
    );
  }
  return (
    <>
      <ScopesLine scopes={scopes} />
      <section className="app-int-sign-in" aria-label="Sign in">
        <Text as="h3" variant="label" tone="muted">
          Sign in
        </Text>
        {signsInAt(ways, signInAt) && (
          <Text as="p" variant="caption" tone="muted">
            {signsInAt(ways, signInAt)}
          </Text>
        )}
        <ErrorBanner error={error} onDismiss={() => setError(null)} />
        {body}
      </section>
    </>
  );
}

/** The redirect address to register with the service for browser sign-in, read-only, with Copy. */
function RedirectAddress({ url }: { url: string }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    void navigator.clipboard?.writeText(url).then(
      () => setCopied(true),
      () => setCopied(false),
    );
  };
  return (
    <div className="app-int-redirect">
      <TextField label="Redirect address" hint="Register this address as the app’s callback URL." value={url} readOnly onFocus={(e) => e.currentTarget.select()} />
      <Button type="button" size="sm" variant="ghost" icon={<Icon name={copied ? 'check' : 'copy'} />} onClick={copy}>
        {copied ? 'Copied' : 'Copy'}
      </Button>
    </div>
  );
}

/** A folded connection card's line: a badge when it needs you, then how many agents use it and since when. */
function connectionMeta(c: ConnectionState, agents: number, now: number): ReactNode {
  return (
    <>
      {c.reconnect_needed && <Badge tone="danger">Reconnect needed</Badge>}
      {c.sign_in?.error && <Badge tone="danger">Sign in again</Badge>}
      {c.verified_at && c.verified === false && <Badge tone="danger">Could not verify</Badge>}
      <span>{agentsLabel(agents)}</span>
      <span aria-hidden="true">·</span>
      <span>{'Connected since ' + whenLabel(c.created_at, now)}</span>
    </>
  );
}

interface ConnectionFormProps {
  page: IntegrationPageData;
  busy: boolean;
  running: boolean;
  now: number;
  onSave(body: ConnectionWrite): Promise<boolean>;
  /** Test connection; absent when the integration has no connection test, and so no button. */
  onVerify?(): void;
  onDisconnect(): void;
  /** The Sign in block's routes and answers, for a connection made by signing in; absent otherwise. */
  signIn?: SignInWiring;
}

/** The connection: one field per entry the bundle asks for. A stored secret shows only that it is stored. */
function ConnectionForm({ page, busy, running, now, onSave, onVerify, onDisconnect, signIn }: ConnectionFormProps) {
  const fields = page.connection_fields;
  const [values, setValues] = useState(() => startingValues(fields));
  const connection = page.connection;
  return (
    <>
      <form
        className="app-int-form"
        onSubmit={(e) => {
          e.preventDefault();
          void onSave(connectionBody(fields, values));
        }}
      >
        {connection?.reconnect_needed && (
          <Banner tone="danger" title="Reconnect needed">
            Its secrets are not stored on this Kivali server, as after a restore from a backup. Enter them again, then save the connection.
          </Banner>
        )}
        {connection && connection.verified_at && !connection.verified && (
          <Banner tone="danger" title="Could not verify the connection">
            {connection.verify_message || 'The integration did not say why.'}
          </Banner>
        )}
        {fields.map((f) => (
          <TextField
            key={f.key}
            label={f.required ? f.title : f.title + ' (optional)'}
            {...(f.description ? { hint: f.description } : {})}
            type={f.sensitive ? 'password' : 'text'}
            autoComplete="off"
            {...(f.sensitive && f.set ? { placeholder: 'Stored' } : {})}
            value={values[f.key] ?? ''}
            onChange={(e) => {
              const v = e.currentTarget.value;
              setValues((m) => ({ ...m, [f.key]: v }));
            }}
          />
        ))}
        <div className="app-int-status">
          {connectionLines(connection, now).map((line) => (
            <Text key={line} as="p" variant="caption" tone="muted">
              {line}
            </Text>
          ))}
          <SignInLine connection={connection} now={now} />
        </div>
        <div className="app-org-actions">
          <Button type="submit" variant="primary" disabled={busy}>
            Save connection
          </Button>
          {onVerify && (
            <>
              <Button type="button" variant="secondary" disabled={busy || !connection || !running} onClick={onVerify}>
                Test connection
              </Button>
              {!running && (
                <Text variant="caption" tone="muted">
                  Not available while it is not running.
                </Text>
              )}
            </>
          )}
          <Button type="button" variant="ghost" disabled={busy || !connection} onClick={onDisconnect}>
            Disconnect
          </Button>
        </div>
      </form>
      {signIn && <SignInBlock connection={connection} fields={fields} busy={busy} now={now} {...signIn} />}
    </>
  );
}

/** "Dana", "Dana and Lee", "Dana, Lee and Kim". */
export function namesInWords(names: readonly string[]): string {
  if (names.length <= 1) return names[0] ?? '';
  return names.slice(0, -1).join(', ') + ' and ' + names.at(-1);
}

/** What deleting one of several connections takes with it: its secrets, and the access of the agents who use it. */
export function deleteConnectionWords(agents: readonly string[]): string {
  const lead = 'Its stored secrets are deleted. ';
  if (agents.length === 0) return lead + 'No agent’s access uses it.';
  return lead + namesInWords(agents) + (agents.length === 1 ? ' loses its access, which uses it.' : ' lose their access, which uses it.');
}

/** The form a new connection starts from: the connection form with nothing stored. */
function blankFields(fields: readonly ConnectionField[]): ConnectionField[] {
  return fields.map(({ value: _value, ...f }) => ({ ...f, set: false }));
}

interface ConnectionCardProps {
  /** The connection; absent for one being added. */
  connection?: ConnectionState;
  /** The connection form with what this connection holds. */
  fields: readonly ConnectionField[];
  /** How many agents' access uses the connection. */
  agents?: number;
  /** Whether a stored connection's card starts open; one being added is always open. */
  open?: boolean;
  busy: boolean;
  running: boolean;
  now: number;
  onSave(body: ConnectionWrite): Promise<boolean>;
  /** Test connection; absent when the integration has no connection test, and so no button. */
  onVerify?(): void;
  onDelete?(): void;
  onCancel?(): void;
  /** The Sign in block's routes and answers, for a stored connection made by signing in; absent otherwise. */
  signIn?: SignInWiring;
}

/**
 * One of several connections, as a card that folds to its label and a line saying whether it needs you, how many
 * agents use it and since when it is connected. Open, it holds its fields (a stored secret shows only that it is
 * stored), what Test connection last found, and Save, Test connection (when the integration has one) and Delete. A
 * connection being added is the same form blank, always open, with Add connection and Cancel.
 */
function ConnectionCard({ connection, fields, agents = 0, open = false, busy, running, now, onSave, onVerify, onDelete, onCancel, signIn }: ConnectionCardProps) {
  const [label, setLabel] = useState(connection?.label ?? '');
  // The label is sent only once typed into: a label the integration derives (Git's host) is not stored, so it
  // follows the fields; sending it back on every save would fix it in place.
  const [labelEdited, setLabelEdited] = useState(false);
  const [values, setValues] = useState(() => startingValues(fields));
  const title = connection ? connection.label || 'Connection' : 'New connection';
  const verified = connection && verifiedLine(connection, now);
  return (
    <Card title={title} {...(connection ? { collapsible: true, defaultOpen: open, meta: connectionMeta(connection, agents, now) } : {})}>
      <form
        className="app-int-form"
        onSubmit={(e) => {
          e.preventDefault();
          void onSave({ ...connectionBody(fields, values), ...(labelEdited ? { label: label.trim() } : {}) });
        }}
      >
        {connection?.reconnect_needed && (
          <Banner tone="danger" title="Reconnect needed">
            Its secrets are not stored on this Kivali server, as after a restore from a backup. Enter them again, then save the connection.
          </Banner>
        )}
        {connection && connection.verified_at && !connection.verified && (
          <Banner tone="danger" title="Could not verify the connection">
            {connection.verify_message || 'The integration did not say why.'}
          </Banner>
        )}
        <TextField label="Label" hint="What you call this connection, such as Company GitLab." autoComplete="off" value={label}
          onChange={(e) => {
            setLabel(e.currentTarget.value);
            setLabelEdited(true);
          }}
        />
        {fields.map((f) => (
          <TextField
            key={f.key}
            label={f.required ? f.title : f.title + ' (optional)'}
            {...(f.description ? { hint: f.description } : {})}
            type={f.sensitive ? 'password' : 'text'}
            autoComplete="off"
            {...(f.sensitive && f.set ? { placeholder: 'Stored' } : {})}
            value={values[f.key] ?? ''}
            onChange={(e) => {
              const v = e.currentTarget.value;
              setValues((m) => ({ ...m, [f.key]: v }));
            }}
          />
        ))}
        {verified && (
          <Text as="p" variant="caption" tone="muted">
            {verified}
          </Text>
        )}
        <SignInLine connection={connection} now={now} />
        <div className="app-org-actions">
          <Button type="submit" variant="primary" disabled={busy}>
            {connection ? 'Save connection' : 'Add connection'}
          </Button>
          {connection ? (
            <>
              {onVerify && (
                <>
                  <Button type="button" variant="secondary" disabled={busy || !running} onClick={onVerify}>
                    Test connection
                  </Button>
                  {!running && (
                    <Text variant="caption" tone="muted">
                      Not available while it is not running.
                    </Text>
                  )}
                </>
              )}
              <Button type="button" variant="ghost" disabled={busy} onClick={onDelete}>
                Delete
              </Button>
            </>
          ) : (
            <Button type="button" variant="ghost" disabled={busy} onClick={onCancel}>
              Cancel
            </Button>
          )}
        </div>
      </form>
      {signIn && connection && <SignInBlock connection={connection} fields={fields} busy={busy} now={now} {...signIn} />}
    </Card>
  );
}

interface VersionRowProps {
  name: string;
  v: IntegrationVersionSummary;
  now: number;
  busy: boolean;
  /** A built-in integration's versions come with Kivali: none offers Roll back. */
  builtin: boolean;
  onRollBack(): void;
}

/** One version: its row opens its install screen; a superseded one has Roll back beside it. */
function VersionRow({ name, v, now, busy, builtin, onRollBack }: VersionRowProps) {
  const href = useHref(versionPath(name, v.version));
  const notes = v.notes_md.trim().split('\n')[0] ?? '';
  const row = (
    <ListRow
      className="app-int-row"
      title={'Version ' + v.version}
      meta={
        <>
          <VersionBadge state={v.state} />
          <span>{'Published by ' + v.published_by.name + ' · ' + whenLabel(v.published_at, now)}</span>
          {notes && <span className="app-int-excerpt">{notes.length > 80 ? notes.slice(0, 79) + '…' : notes}</span>}
        </>
      }
      trail={<Icon name="chevron-right" />}
      href={href}
    />
  );
  if (v.state !== 'superseded' || builtin) return row;
  // The button sits beside the row's link, never inside it.
  return (
    <div className="app-int-version">
      {row}
      <Button size="sm" variant="ghost" aria-label={'Roll back to version ' + v.version} disabled={busy} onClick={onRollBack}>
        Roll back
      </Button>
    </div>
  );
}

/** "From version 1.1.0 to version 1.0.0": the audit's reason, set as a sentence. */
function sentence(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/** A call's outcome in words: allowed, failed with what went wrong, or refused with why; a roll back names its versions. */
export function auditResult(row: Pick<IntegrationAuditRow, 'agent' | 'action' | 'allowed' | 'reason'>): string {
  if (isRollBack(row)) return sentence(row.reason);
  if (!row.allowed) return row.reason ? 'Refused: ' + row.reason : 'Refused';
  return row.reason ? 'Failed: ' + row.reason : 'Allowed';
}

/** The audit's action in words: "Create post", or "Roll back" for yours. */
function auditAction(row: Pick<IntegrationAuditRow, 'agent' | 'action'>): string {
  return isRollBack(row) ? 'Roll back' : actionWords(row.action);
}

const LOG_LEVEL: Record<string, { tone: BadgeTone; variant?: 'outline' } | undefined> = {
  stderr: { tone: 'neutral' },
  warning: { tone: 'signal' },
  error: { tone: 'danger' },
  // Detail, such as apt-get's output while the integration's software installs: the quietest badge.
  debug: { tone: 'neutral', variant: 'outline' },
};

/** One log line: its time, its level as a small badge (none for info), its message in mono. */
function LogRow({ line, now }: { line: IntegrationLogLine; now: number }) {
  const badge = LOG_LEVEL[line.level];
  return (
    <li className="app-int-log-line">
      <span className="app-int-nowrap app-int-log-time">{whenLabel(line.at, now)}</span>
      <span className="app-int-log-level">
        {badge && (
          <Badge tone={badge.tone} variant={badge.variant}>
            {line.level}
          </Badge>
        )}
      </span>
      <span className="app-int-log-message">{line.message}</span>
    </li>
  );
}

/** The integration's latest log lines, fetched on mount and again on Refresh. */
function LogBlock({ api, now }: { api: string; now: number }) {
  const [logs, setLogs] = useState<IntegrationLogs | null>(null);
  const [error, setError] = useState<ReturnType<typeof asApiError> | null>(null);
  const [seq, setSeq] = useState(0);
  useEffect(() => {
    const ctl = new AbortController();
    apiGet<IntegrationLogs>(api + '/logs', { signal: ctl.signal }).then(
      (v) => {
        setLogs(v);
        setError(null);
      },
      (err: unknown) => {
        if (err instanceof DOMException && err.name === 'AbortError') return;
        setError(asApiError(err));
      },
    );
    return () => ctl.abort();
  }, [api, seq]);
  return (
    <Block
      label="Recent logs"
      variant="heading"
      action={
        <Button size="sm" variant="ghost" onClick={() => setSeq((n) => n + 1)}>
          Refresh
        </Button>
      }
    >
      <ErrorBanner error={error} />
      {!logs && !error && <Skeleton height={64} />}
      {logs && logs.lines.length === 0 && <None>Nothing logged yet.</None>}
      {logs && logs.lines.length > 0 && (
        <ul className="app-int-log">
          {logs.lines.map((l, i) => (
            <LogRow key={i} line={l} now={now} />
          ))}
        </ul>
      )}
    </Block>
  );
}

interface Grant {
  agent: PersonRef;
  row?: AccessRow;
}

/**
 * An integration's page under Org: whether it is on, its connection, which agents have access and how far, its
 * actions, its versions, its recent calls and its recent logs. Every change answers with the whole page.
 */
export function IntegrationPage() {
  const { name = '' } = useParams();
  const navigate = useNavigate();
  const location = useLocation();
  // How a browser sign-in came back here, read once: the query is taken off the address so a reload does not show it
  // again.
  const [returned] = useState(() => signInReturnOf(location.search));
  const api = integrationApi(name);
  const page = useResource<IntegrationPageData>(api);
  const audit = useResource<IntegrationAudit>(api + '/audit');
  const action = useAction();
  const [now] = useState(() => Date.now());
  const [removing, setRemoving] = useState(false);
  const [disconnecting, setDisconnecting] = useState(false);
  // Several connections: the one Delete asks about, and whether a new one is being added.
  const [deleting, setDeleting] = useState<ConnectionState | null>(null);
  const [adding, setAdding] = useState(false);
  const [grant, setGrant] = useState<Grant | null>(null);
  const [rollingBack, setRollingBack] = useState<string | null>(null);
  const [pick, setPick] = useState('');
  // Bumped after a save so the connection form starts again from what is stored.
  const [formSeq, setFormSeq] = useState(0);
  // The same for each connection's card, by id, and for the card that adds one.
  const [cardSeq, setCardSeq] = useState<Record<string, number>>({});
  const [addSeq, setAddSeq] = useState(0);
  const p = page.data;

  useFrameChrome({ title: p?.summary.display_name ?? 'Integration', back: { to: '/org/integrations', label: 'Integrations' } });
  useEffect(() => {
    if (returned && location.search) void navigate({ pathname: location.pathname, search: '' }, { replace: true });
  }, [returned, location.search, location.pathname, navigate]);

  const mutate = (fn: () => Promise<IntegrationPageData>) =>
    action.run(async () => {
      page.set(await fn());
    });
  const toggle = (on: boolean) => mutate(() => apiPost<IntegrationPageData>(api + (on ? '/enable' : '/disable')));
  const save = async (body: ConnectionWrite) => {
    const ok = await mutate(() => apiPut<IntegrationPageData>(api + '/connection', body));
    if (ok) setFormSeq((n) => n + 1);
    return ok;
  };
  const verify = () => void mutate(() => apiPost<IntegrationPageData>(api + '/connection/verify'));
  const disconnect = async () => {
    const ok = await mutate(() => apiDelete<IntegrationPageData>(api + '/connection'));
    setDisconnecting(false);
    if (ok) setFormSeq((n) => n + 1);
  };
  const connectionApi = (id: string) => api + '/connections/' + encodeURIComponent(id);
  // Several connections: only the card that saved starts again from what is stored, so another card keeps what was
  // typed into it.
  const saveConnection = async (id: string, body: ConnectionWrite) => {
    const ok = await mutate(() => apiPut<IntegrationPageData>(connectionApi(id), body));
    if (ok) setCardSeq((m) => ({ ...m, [id]: (m[id] ?? 0) + 1 }));
    return ok;
  };
  const addConnection = async (body: ConnectionWrite) => {
    const ok = await mutate(() => apiPost<IntegrationPageData>(api + '/connections', body));
    if (ok) {
      setAdding(false);
      setAddSeq((n) => n + 1);
    }
    return ok;
  };
  // A sign-in or a sign-out changes the page and adds a row to the audit, which are read again.
  const signedIn = () =>
    action.run(async () => {
      page.set(await apiGet<IntegrationPageData>(api));
      audit.set(await apiGet<IntegrationAudit>(api + '/audit'));
    });
  const signedOut = (next: IntegrationPageData) => {
    page.set(next);
    void apiGet<IntegrationAudit>(api + '/audit').then(audit.set, () => {});
  };
  // A browser sign-in that came back goes to its connection's block: the one connection's, or the one it names.
  const returnedTo = (id: string | undefined) =>
    returned && (!p?.multiple || (returned.kind !== 'start_again' && returned.id === id)) ? returned : null;
  const signInFor = (connectionApi: string, id?: string): { signIn?: SignInWiring } =>
    p && (p.connection_method === 'device' || p.connection_method === 'browser')
      ? {
          signIn: {
            api: connectionApi,
            scopes: p.scopes,
            ...(p.sign_in_at ? { signInAt: p.sign_in_at } : {}),
            onPage: signedOut,
            onSignedIn: signedIn,
            returned: returnedTo(id),
          },
        }
      : {};
  const verifyConnection = (id: string) => void mutate(() => apiPost<IntegrationPageData>(connectionApi(id) + '/verify'));
  const deleteConnection = async (id: string) => {
    await mutate(() => apiDelete<IntegrationPageData>(connectionApi(id)));
    setDeleting(null);
  };
  const revoke = (slug: string) => void mutate(() => apiDelete<IntegrationPageData>(api + '/access/' + encodeURIComponent(slug)));
  const remove = () =>
    action.run(async () => {
      await apiDelete(api);
      void navigate('/org/integrations');
    });
  // The roll back answers with the version; the page and the audit (which gains a row for it) are read again.
  const rollBack = (version: string) =>
    action.run(async () => {
      await apiPost<IntegrationVersion>(rollBackApi(name, version));
      setRollingBack(null);
      page.set(await apiGet<IntegrationPageData>(api));
      audit.set(await apiGet<IntegrationAudit>(api + '/audit'));
    });

  if (!p) {
    return (
      <div className="app-int" aria-busy={page.error ? undefined : true}>
        <ErrorBanner error={page.error} />
        {!page.error && (
          <>
            <Skeleton width="40%" height={28} />
            <Skeleton height={120} />
            <Skeleton height={160} />
          </>
        )}
      </div>
    );
  }

  const s = p.summary;
  const pending = s.pending_version;
  const invalid = invalidAccessLine(p.access.filter((r) => r.invalid).length);
  const chosen = p.agents_without.some((a) => a.slug === pick) ? pick : (p.agents_without[0]?.slug ?? '');
  // What needs the integration running waits while it is not. A built-in one runs inside Kivali and is never
  // not running, so Test connection is there as soon as it is connected; one this version of Kivali does not
  // carry never runs.
  const running = s.status !== 'not_running' && s.status !== 'not_included';

  const accessColumns: TableColumn<AccessRow & { id: string }>[] = [
    { key: 'agent', label: 'Agent', render: (r) => <span className="app-int-nowrap">{r.agent.name}</span> },
    // Which of several connections each agent's calls use.
    ...(p.multiple
      ? [{ key: 'connection', label: 'Connection', render: (r: AccessRow) => <span className="app-int-nowrap">{r.connection_label || 'None'}</span> }]
      : []),
    {
      key: 'summary',
      label: 'Access',
      render: (r) => (
        <div className="app-int-cell">
          <span>{r.summary}</span>
          {r.invalid && (
            <Text variant="caption" tone="danger">
              {'Invalid: ' + r.invalid + '. It grants nothing until you change it.'}
            </Text>
          )}
        </div>
      ),
    },
    { key: 'set_at', label: 'Set', mono: true, render: (r) => <span className="app-int-nowrap">{whenLabel(r.set_at, now)}</span> },
    {
      key: 'actions',
      label: '',
      align: 'right',
      render: (r) => (
        <span className="app-int-row-actions">
          <Button size="sm" variant="secondary" aria-label={'Change ' + r.agent.name + '’s access'} disabled={action.busy} onClick={() => setGrant({ agent: r.agent, row: r })}>
            Change
          </Button>
          <Button size="sm" variant="ghost" aria-label={'Remove ' + r.agent.name + '’s access'} disabled={action.busy} onClick={() => revoke(r.agent.slug)}>
            Remove
          </Button>
        </span>
      ),
    },
  ];

  const auditColumns: TableColumn<IntegrationAuditRow & { id: number }>[] = [
    { key: 'at', label: 'When', mono: true, render: (r) => <span className="app-int-nowrap">{whenLabel(r.at, now)}</span> },
    { key: 'agent', label: 'Agent', render: (r) => <span className="app-int-nowrap">{r.agent.name}</span> },
    { key: 'action', label: 'Action', render: (r) => <span className="app-int-nowrap">{auditAction(r)}</span> },
    { key: 'result', label: 'Result', render: (r) => <span className="app-int-wide">{auditResult(r)}</span> },
    { key: 'duration_ms', label: 'Took', mono: true, align: 'right', render: (r) => <span className="app-int-nowrap">{durationLabel(r.duration_ms)}</span> },
  ];

  return (
    <div className="app-int">
      <header className="app-int-head">
        <PageTitle>{s.display_name}</PageTitle>
        <div className="app-int-head-row">
          <StatusBadge status={s.status} />
          {s.builtin && <Badge variant="outline">Built in</Badge>}
          <Text variant="caption" tone="muted">
            {s.version ? 'Version ' + s.version : 'No version approved yet'}
          </Text>
          <span className="app-int-spacer" />
          <Switch label="On" checked={s.enabled} disabled={action.busy} onCheckedChange={(on) => void toggle(on)} />
          {p.can_remove && (
            <Button variant="ghost" icon={<Icon name="trash2" />} disabled={action.busy} onClick={() => setRemoving(true)}>
              Remove
            </Button>
          )}
        </div>
        {s.status === 'not_running' && (
          <Text as="p" variant="caption" tone="muted">
            Kivali is starting it, or it stopped. The log below says what happened.
          </Text>
        )}
        {s.status === 'not_included' && (
          <Text as="p" variant="caption" tone="muted">
            Another version of Kivali shipped it, and this one does not include it. Agents cannot use it; you can remove it.
          </Text>
        )}
        {s.description && (
          <Text as="p" variant="body">
            {s.description}
          </Text>
        )}
      </header>

      <ErrorBanner error={action.error} onDismiss={action.dismiss} />

      {pending && (
        <Banner
          tone="warning"
          title={'Version ' + pending + ' waits for your approval'}
          action={
            <Button size="sm" variant="secondary" onClick={() => void navigate(versionPath(name, pending))}>
              Review
            </Button>
          }
        >
          Nothing in it runs until you approve it.
        </Banner>
      )}

      {invalid && <Banner tone="warning">{invalid}</Banner>}

      <Block label={p.multiple ? 'Connections' : 'Connection'} variant="heading">
        {/* What to do on the vendor's side first, from the manifest; nothing when it has none. */}
        {s.version && <Markdown markdown={p.setup_notes_md} />}
        {s.version && p.sign_in_redirect_url && <RedirectAddress url={p.sign_in_redirect_url} />}
        {p.multiple && returned?.kind === 'start_again' && (
          <Banner tone="danger" title="The sign-in did not work">
            {returned.words}
          </Banner>
        )}
        {s.version && p.multiple ? (
          <div className="app-int-conns">
            {p.connections.length === 0 && !adding && <None>No connections yet.</None>}
            {p.connections.map((c) => (
              <ConnectionCard
                key={c.id + ':' + (cardSeq[c.id] ?? 0)}
                connection={c}
                fields={c.fields}
                agents={p.access.filter((a) => a.connection_id === c.id).length}
                // A card remounts after its own save (cardSeq), which it can only have while open: it stays open.
                open={(cardSeq[c.id] ?? 0) > 0}
                busy={action.busy}
                running={running}
                now={now}
                onSave={(body) => saveConnection(c.id, body)}
                {...(p.connection_test ? { onVerify: () => verifyConnection(c.id) } : {})}
                onDelete={() => setDeleting(c)}
                {...signInFor(connectionApi(c.id), c.id)}
              />
            ))}
            {adding ? (
              <ConnectionCard
                key={'new:' + addSeq}
                fields={blankFields(p.connection_fields)}
                busy={action.busy}
                running={running}
                now={now}
                onSave={addConnection}
                onCancel={() => setAdding(false)}
              />
            ) : (
              <div className="app-org-actions">
                <Button variant="secondary" icon={<Icon name="plus" />} disabled={action.busy} onClick={() => setAdding(true)}>
                  Add a connection
                </Button>
              </div>
            )}
          </div>
        ) : s.version ? (
          <ConnectionForm
            key={formSeq}
            page={p}
            busy={action.busy}
            running={running}
            now={now}
            onSave={save}
            {...(p.connection_test ? { onVerify: verify } : {})}
            onDisconnect={() => setDisconnecting(true)}
            {...signInFor(api + '/connection')}
          />
        ) : (
          <Text as="p" variant="body" tone="muted">
            Approve a version first.
          </Text>
        )}
      </Block>

      <Block label="Access" variant="heading">
        {p.access.length === 0 ? (
          <None>No agent has access yet.</None>
        ) : (
          <Table columns={accessColumns} rows={p.access.map((r) => ({ ...r, id: r.agent.slug }))} />
        )}
        {p.agents_without.length > 0 && (
          <div className="app-org-add">
            <div className="app-org-add-field">
              <Select label="Agent" value={chosen} onChange={(e) => setPick(e.currentTarget.value)} options={p.agents_without.map((a) => ({ value: a.slug, label: a.name }))} />
            </div>
            <div className="app-org-add-action">
              <Button
                variant="secondary"
                disabled={action.busy || !chosen}
                onClick={() => {
                  const agent = p.agents_without.find((a) => a.slug === chosen);
                  if (agent) setGrant({ agent });
                }}
              >
                Give access
              </Button>
            </div>
          </div>
        )}
      </Block>

      {/* A bundle's, uploaded or shipped; a built-in of Go code installs none. */}
      {p.packages.length > 0 && (
        <Block label="Software" variant="heading">
          <ul className="app-int-facts">
            {p.packages.map((pkg) => (
              <li key={pkg}>
                <Text variant="code">{pkg}</Text>
              </li>
            ))}
          </ul>
          <Text as="p" variant="caption" tone="muted">
            Debian packages, installed each time the integration starts.
          </Text>
        </Block>
      )}

      <Block label="Actions" variant="heading">
        {p.actions.length === 0 ? (
          <None>No actions.</None>
        ) : (
          <ul className="app-int-facts">
            {p.actions.map((a) => (
              <li key={a.name} className="app-int-fact">
                <Text variant="body">{actionWords(a.name)}</Text>
                {a.description && (
                  <Text variant="caption" tone="muted">
                    {a.description}
                  </Text>
                )}
              </li>
            ))}
          </ul>
        )}
      </Block>

      <Block label="Versions" variant="heading">
        {p.versions.length === 0 ? (
          <None>No versions.</None>
        ) : (
          <RowList>
            {p.versions.map((v) => (
              <VersionRow key={v.version} name={name} v={v} now={now} busy={action.busy} builtin={s.builtin} onRollBack={() => setRollingBack(v.version)} />
            ))}
          </RowList>
        )}
      </Block>

      <Block label="Recent calls" variant="heading">
        <ErrorBanner error={audit.error} />
        {!audit.data && !audit.error && <Skeleton height={64} />}
        {audit.data && audit.data.rows.length === 0 && <None>No calls yet.</None>}
        {audit.data && audit.data.rows.length > 0 && <Table dense columns={auditColumns} rows={audit.data.rows.map((r, i) => ({ ...r, id: i }))} />}
      </Block>

      <LogBlock api={api} now={now} />

      {grant && (
        <AccessDialog
          running={running}
          name={name}
          displayName={s.display_name}
          agent={grant.agent}
          form={p.access_form}
          connections={p.connections}
          multiple={p.multiple}
          {...(grant.row ? { access: grant.row.access as unknown, connectionId: grant.row.connection_id } : {})}
          onClose={() => setGrant(null)}
          onSaved={(next) => {
            page.set(next);
            setGrant(null);
          }}
        />
      )}

      <RollBackDialog
        version={rollingBack}
        current={s.version}
        busy={action.busy}
        error={action.error && <ErrorBanner error={action.error} />}
        onConfirm={() => {
          if (rollingBack) void rollBack(rollingBack);
        }}
        onClose={() => setRollingBack(null)}
      />
      <Dialog
        open={disconnecting}
        onOpenChange={(o) => {
          if (!o) setDisconnecting(false);
        }}
        tone="danger"
        title={'Disconnect ' + s.display_name + '?'}
        description="Its stored connection is deleted, along with every agent’s access to it. Connect it again, then give access again."
        footer={
          <>
            <DialogClose>
              <Button variant="secondary">Cancel</Button>
            </DialogClose>
            <Button variant="danger" onClick={() => void disconnect()} disabled={action.busy}>
              Disconnect
            </Button>
          </>
        }
      />
      <Dialog
        open={deleting !== null}
        onOpenChange={(o) => {
          if (!o) setDeleting(null);
        }}
        tone="danger"
        title={'Delete ' + (deleting?.label || 'this connection') + '?'}
        description={deleteConnectionWords(p.access.filter((r) => r.connection_id === deleting?.id).map((r) => r.agent.name))}
        footer={
          <>
            <DialogClose>
              <Button variant="secondary">Cancel</Button>
            </DialogClose>
            <Button variant="danger" onClick={() => deleting && void deleteConnection(deleting.id)} disabled={action.busy}>
              Delete
            </Button>
          </>
        }
      />
      <Dialog
        open={removing}
        onOpenChange={(o) => {
          if (!o) setRemoving(false);
        }}
        tone="danger"
        title={'Remove ' + s.display_name + '?'}
        description="Its connection and every agent’s access go with it. A new bundle can install it again."
        footer={
          <>
            <DialogClose>
              <Button variant="secondary">Cancel</Button>
            </DialogClose>
            <Button variant="danger" onClick={() => void remove()} disabled={action.busy}>
              Remove
            </Button>
          </>
        }
      >
        {action.error && <ErrorBanner error={action.error} />}
      </Dialog>
    </div>
  );
}
