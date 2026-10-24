import { useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { useHref, useLocation, useNavigate, useParams } from 'react-router';
import { apiDelete, apiGet, apiPost, apiPut } from '../../api/client';
import type { ApiError } from '../../api/client';
import type {
  AccessRow,
  ConnectionField,
  ConnectionState,
  ConnectionWrite,
  IntegrationAudit,
  IntegrationAuditRow,
  IntegrationLogLine,
  IntegrationLogs,
  IntegrationPage as IntegrationPageData,
  IntegrationRequirement,
  IntegrationVersion,
  IntegrationVersionSummary,
  PersonRef,
} from '../../api/types.gen';
import { useFrameChrome } from '../../app/chrome';
import { PageTitle } from '../../app/PageTitle';
import { Badge, Banner, Button, Dialog, DialogClose, Icon, ListRow, Select, Skeleton, Switch, Table, Text, TextField } from '../../ds';
import type { BadgeTone, TableColumn } from '../../ds';
import { AccessDialog } from './AccessDialog';
import { SetupFlowPanel } from './SetupFlowPanel';
import {
  Block,
  Markdown,
  RollBackDialog,
  ScopesLine,
  SetupFacts,
  NeedsBadge,
  StateBadge,
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

/**
 * The flow a service's callback sent the browser back with: ?setup=<id>, a flow id (32 hex digits), or null. The query
 * never carries words, since anyone can link to the page with any query: the page reads how the flow stands from the
 * server.
 */
export function setupReturnOf(search: string): string | null {
  const id = new URLSearchParams(search).get('setup');
  return id && /^[0-9a-f]{32}$/.test(id) ? id : null;
}

interface SignInBlockProps {
  /** The connection, made by signing in. */
  connection: ConnectionState;
  /** The integration's routes: …/integrations/{name}. Sign out is the connection's, …/connection/sign-out. */
  api: string;
  busy: boolean;
  /** The page as Sign out answers it. */
  onPage(page: IntegrationPageData): void;
}

/**
 * A connection made by signing in, under its form: signed in, with who the connection test found and Sign out; or,
 * not signed in where no way of signing in works from here, where to sign in from instead. Signing in again is the
 * sign-in requirement's Fix, above, so this block starts no flow.
 */
function SignInBlock({ connection, api, busy, onPage }: SignInBlockProps) {
  const [error, setError] = useState<ApiError | null>(null);
  const [working, setWorking] = useState(false);
  const s = connection.sign_in;
  const signOut = async () => {
    setWorking(true);
    setError(null);
    try {
      onPage(await apiPost<IntegrationPageData>(api + '/connection/sign-out'));
    } catch (err) {
      setError(asApiError(err));
    } finally {
      setWorking(false);
    }
  };

  let body: ReactNode;
  if (s?.signed_in) {
    const who = connection.verified && connection.verify_message;
    body = (
      <>
        <Text as="p" variant="body">
          {who ? 'Signed in. ' + who : 'Signed in.'}
        </Text>
        <div className="app-org-actions">
          <Button type="button" variant="secondary" disabled={busy || working} onClick={() => void signOut()}>
            Sign out
          </Button>
        </div>
      </>
    );
  } else if ((s?.ways.length ?? 0) === 0 && s?.browser_elsewhere) {
    body = (
      <>
        <Text as="p" variant="body">
          {s.open_at
            ? 'Signing in does not work from here. Open the team’s address in your browser and sign in there.'
            : 'Signing in does not work from here. Open the team in your browser, at its own address, and sign in there.'}
        </Text>
        {s.open_at && <CopyAddress label="Team address" url={s.open_at} />}
      </>
    );
  } else if ((s?.ways.length ?? 0) === 0) {
    // The ways offered are this server's; a manifest may name only ways it does not have.
    body = (
      <Text as="p" variant="body" tone="muted">
        This version of Kivali has no way to sign in to this service.
      </Text>
    );
  } else {
    return null;
  }
  return (
    <section className="app-int-sign-in" aria-label="Sign in">
      <Text as="h3" variant="label" tone="muted">
        Sign in
      </Text>
      <ErrorBanner error={error} onDismiss={() => setError(null)} />
      {body}
    </section>
  );
}

/** An address to copy, such as the redirect address to register with the service, read-only, with Copy. */
function CopyAddress({ label, hint, url }: { label: string; hint?: string; url: string }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    void navigator.clipboard?.writeText(url).then(
      () => setCopied(true),
      () => setCopied(false),
    );
  };
  return (
    <div className="app-int-redirect">
      <TextField label={label} {...(hint ? { hint } : {})} value={url} readOnly onFocus={(e) => e.currentTarget.select()} />
      <Button type="button" size="sm" variant="ghost" icon={<Icon name={copied ? 'check' : 'copy'} />} onClick={copy}>
        {copied ? 'Copied' : 'Copy'}
      </Button>
    </div>
  );
}

interface ConnectionFormProps {
  page: IntegrationPageData;
  busy: boolean;
  now: number;
  onSave(body: ConnectionWrite): Promise<boolean>;
  /** Test connection; absent when the integration has no connection test, and so no button. */
  onVerify?(): void;
  /** Sign out's answer, for a connection made by signing in. */
  onPage(page: IntegrationPageData): void;
}

/**
 * An installed integration's one connection: its form, one field per entry the bundle asks for (a stored secret shows
 * only that it is stored), with Save connection; since when it is connected and, made by signing in, when the owner
 * signed in; Test connection where there is one; and, made by signing in, the Sign in block. A connection its own
 * setup made has no form. How it stands (secrets, sign-in, the last test) is in Requirements.
 */
function ConnectionForm({ page, busy, now, onSave, onVerify, onPage }: ConnectionFormProps) {
  const fields = page.connection_fields;
  const [values, setValues] = useState(() => startingValues(fields));
  const connection = page.connection;
  const actions = (
    <>
      {fields.length > 0 && (
        <Button type="submit" variant="primary" disabled={busy}>
          Save connection
        </Button>
      )}
      {onVerify && (
        <>
          <Button type="button" variant="secondary" disabled={busy || !connection || !page.running} onClick={onVerify}>
            Test connection
          </Button>
          {!page.running && (
            <Text variant="caption" tone="muted">
              Not available while it is not running.
            </Text>
          )}
        </>
      )}
    </>
  );
  const status = (
    <div className="app-int-status">
      {connection && (
        <Text as="p" variant="caption" tone="muted">
          {'Connected since ' + whenLabel(connection.created_at, now)}
        </Text>
      )}
      <SignInLine connection={connection} now={now} />
    </div>
  );
  return (
    <>
      {fields.length > 0 ? (
        <form
          className="app-int-form"
          onSubmit={(e) => {
            e.preventDefault();
            void onSave(connectionBody(fields, values));
          }}
        >
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
          {status}
          <div className="app-org-actions">{actions}</div>
        </form>
      ) : (
        <>
          {status}
          {onVerify && <div className="app-org-actions">{actions}</div>}
        </>
      )}
      {connection?.sign_in && <SignInBlock connection={connection} api={integrationApi(page.summary.name)} busy={busy} onPage={onPage} />}
    </>
  );
}

/** A requirement row's icon: met, or not. */
function RequirementRow({ row, busy, onFix }: { row: IntegrationRequirement; busy: boolean; onFix(): void }) {
  return (
    <li className={'app-int-req' + (row.met ? '' : ' is-unmet')}>
      <Icon name={row.met ? 'circle-check' : 'triangle-alert'} className="app-int-req-icon" label={row.met ? 'Met' : 'Not met'} />
      <span className="app-int-req-text">
        <Text variant="body">{row.title}</Text>
        <Text variant="caption" tone="muted">
          {row.words}
        </Text>
      </span>
      {!row.met && row.fix && (
        <Button size="sm" variant="secondary" aria-label={'Fix ' + row.title} disabled={busy} onClick={onFix}>
          Fix
        </Button>
      )}
    </li>
  );
}

/**
 * What Uninstall does, for its dialog: the service's side undone where it can be, the connection and its secrets
 * forgotten, and whose access goes; then, for an integration that is removed (uploaded, or not in this version of
 * Kivali), that it goes whole, or else that it stays available to install again.
 */
export function uninstallWords(display: string, removed: boolean, agents: readonly string[]): string {
  const lead = 'Kivali undoes the installation on the service where it can, then forgets the connection and its secrets. ';
  const access = agents.length === 0 ? 'No agent has access. ' : namesInWords(agents) + (agents.length === 1 ? ' loses its access. ' : ' lose their access. ');
  const after = removed ? display + ' goes with every version and its log; uploading a bundle installs it again.' : display + ' stays available to install again.';
  return lead + access + after;
}

/** "Dana", "Dana and Lee", "Dana, Lee and Kim". */
export function namesInWords(names: readonly string[]): string {
  if (names.length <= 1) return names[0] ?? '';
  return names.slice(0, -1).join(', ') + ' and ' + names.at(-1);
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
      collapsible
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

/** A Fix under way on the page: the flow it runs, the requirement it mends, and a count that starts it again. */
interface Fixing {
  flow: string;
  title: string;
  seq: number;
}

/** The states a flow on the page is still worth drawing in: under way, or failed with why. */
const SHOWN_FLOW: ReadonlySet<string> = new Set(['running', 'form', 'go', 'wait', 'failed']);

/**
 * An integration's page under Org, drawn from its lifecycle state. Available (or setting up): what installing it does
 * and one button, Install, which runs its setup flow. Installed or off: its requirements, each unmet one with its Fix,
 * its connection, which agents have access and how far, and Uninstall. Every state shows its actions, versions,
 * recent calls and recent logs. Every change answers with the whole page.
 */
export function IntegrationPage() {
  const { name = '' } = useParams();
  const navigate = useNavigate();
  const location = useLocation();
  // The flow a service's callback sent the browser back with (?setup=<id>), read once: the query is taken off the
  // address so a reload does not read it again.
  const [returned] = useState(() => setupReturnOf(location.search));
  const api = integrationApi(name);
  const page = useResource<IntegrationPageData>(api);
  const audit = useResource<IntegrationAudit>(api + '/audit');
  const action = useAction();
  const [now] = useState(() => Date.now());
  const [uninstalling, setUninstalling] = useState(false);
  const [grant, setGrant] = useState<Grant | null>(null);
  const [rollingBack, setRollingBack] = useState<string | null>(null);
  const [fixing, setFixing] = useState<Fixing | null>(null);
  const [pick, setPick] = useState('');
  // Bumped after a save so the connection form starts again from what is stored.
  const [formSeq, setFormSeq] = useState(0);
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
  // A flow that finished, or a sign-out, changes the page and adds a row to the audit, which are read again.
  const flowDone = () =>
    action.run(async () => {
      page.set(await apiGet<IntegrationPageData>(api));
      audit.set(await apiGet<IntegrationAudit>(api + '/audit'));
      setFixing(null);
    });
  const signedOut = (next: IntegrationPageData) => {
    page.set(next);
    void apiGet<IntegrationAudit>(api + '/audit').then(audit.set, () => {});
  };
  const revoke = (slug: string) => void mutate(() => apiDelete<IntegrationPageData>(api + '/access/' + encodeURIComponent(slug)));
  // Uninstall takes an uploaded integration (or one this version of Kivali does not carry) off the team, back to the
  // list; a built-in stays, available again, and its page and audit are read again.
  const uninstall = (removed: boolean) =>
    action.run(async () => {
      await apiDelete(api);
      setUninstalling(false);
      if (removed) {
        void navigate('/org/integrations');
        return;
      }
      page.set(await apiGet<IntegrationPageData>(api));
      audit.set(await apiGet<IntegrationAudit>(api + '/audit'));
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
  const state = s.state;
  const installed = state === 'installed' || state === 'off';
  const available = state === 'available' || state === 'setting_up';
  const removed = !s.builtin || state === 'not_included';
  const pending = s.pending_version;
  const invalid = invalidAccessLine(p.access.filter((r) => r.invalid).length);
  const chosen = p.agents_without.some((a) => a.slug === pick) ? pick : (p.agents_without[0]?.slug ?? '');
  const signIn = p.connection_method === 'device' || p.connection_method === 'browser';
  // The flow a Fix runs: one the owner just started, or one under way (or failed) that the page carries, such as a
  // Go step's service sending the browser back here.
  const fixFlow: Fixing | null =
    fixing ??
    (installed && p.flow && SHOWN_FLOW.has(p.flow.state)
      ? { flow: p.flow.flow, title: p.requirements.find((r) => r.fix === p.flow?.flow)?.title ?? 'Fix', seq: 0 }
      : installed && returned
        ? signIn
          ? { flow: 'sign_in', title: p.requirements.find((r) => r.fix === 'sign_in')?.title ?? 'Fix', seq: 0 }
          : { flow: 'setup', title: 'Fix', seq: 0 }
        : null);
  // Where installing signs in, for a sign-in connection: every way's host, as the install screen names them.
  const installNote = signIn ? signsInAt(Object.keys(p.sign_in_at ?? {}), p.sign_in_at) : undefined;
  // Where signing in again sends the owner, for the sign-in row's Fix.
  const fixNote = fixFlow?.flow === 'sign_in' ? signsInAt((p.connection?.sign_in?.ways ?? []).slice(0, 1), p.sign_in_at) : undefined;

  const accessColumns: TableColumn<AccessRow & { id: string }>[] = [
    { key: 'agent', label: 'Agent', render: (r) => <span className="app-int-nowrap">{r.agent.name}</span> },
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

  // What the vendor's side needs first and what installing does, from the manifest: on the Install block before it
  // runs, and on the Connection block after.
  const declarations = (
    <>
      <Markdown markdown={p.setup_notes_md} />
      {p.setup && <SetupFacts setup={p.setup} integration={s.display_name} />}
      {p.sign_in_redirect_url && <CopyAddress label="Redirect address" hint="Register this address as the app’s callback URL." url={p.sign_in_redirect_url} />}
      <ScopesLine scopes={p.scopes} />
    </>
  );

  return (
    <div className="app-int">
      <header className="app-int-head">
        <PageTitle>{s.display_name}</PageTitle>
        <div className="app-int-head-row">
          <StateBadge state={state} />
          {state === 'installed' && <NeedsBadge needs={s.needs} />}
          {s.builtin && <Badge variant="outline">Built in</Badge>}
          <Text variant="caption" tone="muted">
            {s.version ? 'Version ' + s.version : 'No version approved yet'}
          </Text>
          <span className="app-int-spacer" />
          {installed && <Switch label="On" checked={s.enabled} disabled={action.busy} onCheckedChange={(on) => void toggle(on)} />}
          {p.can_uninstall && (
            <Button variant="ghost" icon={<Icon name="trash2" />} disabled={action.busy} onClick={() => setUninstalling(true)}>
              Uninstall
            </Button>
          )}
        </div>
        {state === 'not_included' && (
          <Text as="p" variant="caption" tone="muted">
            Another version of Kivali shipped it, and this one does not include it. Agents cannot use it; you can uninstall it.
          </Text>
        )}
        {state === 'off' && (
          <Text as="p" variant="caption" tone="muted">
            Switched off: agents cannot use it. Its connection and every agent’s access are kept for when you switch it on.
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

      {state === 'pending' && (
        <Block label="Install" variant="heading">
          <Text as="p" variant="body" tone="muted">
            Approve a version first.
          </Text>
        </Block>
      )}

      {available && (
        <Block label="Install" variant="heading">
          {/* An integration whose connection an earlier version made, which this version's setup does not take, says
              so here: installing it again is the Fix. */}
          {p.requirements
            .filter((r) => !r.met)
            .map((r) => (
              <Banner key={r.id} tone="warning">
                {r.words}
              </Banner>
            ))}
          {declarations}
          <SetupFlowPanel
            api={api}
            integration={s.display_name}
            label="Setup"
            startLabel="Install"
            busy={action.busy}
            returned={returned}
            {...(installNote ? { note: installNote } : {})}
            onDone={flowDone}
          />
        </Block>
      )}

      {installed && (
        <Block label="Requirements" variant="heading">
          <ul className="app-int-reqs">
            {p.requirements.map((r) => (
              <RequirementRow
                key={r.id}
                row={r}
                busy={action.busy}
                onFix={() => setFixing((f) => ({ flow: r.fix, title: r.title, seq: (f?.seq ?? 0) + 1 }))}
              />
            ))}
          </ul>
          {fixFlow && (
            <SetupFlowPanel
              key={fixFlow.flow + ':' + fixFlow.seq}
              api={api}
              integration={s.display_name}
              label={fixFlow.title}
              flow={fixFlow.flow}
              startLabel="Fix"
              autoStart={fixFlow.seq > 0}
              {...(fixNote ? { note: fixNote } : {})}
              busy={action.busy}
              {...(fixFlow.seq === 0 ? { returned } : {})}
              onDone={flowDone}
            />
          )}
        </Block>
      )}

      {installed && (
        <Block label="Connection" variant="heading">
          {declarations}
          <ConnectionForm
            key={formSeq}
            page={p}
            busy={action.busy}
            now={now}
            onSave={save}
            {...(p.connection_test ? { onVerify: verify } : {})}
            onPage={signedOut}
          />
        </Block>
      )}

      {installed && (
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
      )}

      {/* A bundle's; a built-in installs none. */}
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

      <Block label="Recent calls" variant="heading" collapsible>
        <ErrorBanner error={audit.error} />
        {!audit.data && !audit.error && <Skeleton height={64} />}
        {audit.data && audit.data.rows.length === 0 && <None>No calls yet.</None>}
        {audit.data && audit.data.rows.length > 0 && <Table dense columns={auditColumns} rows={audit.data.rows.map((r, i) => ({ ...r, id: i }))} />}
      </Block>

      <LogBlock api={api} now={now} />

      {grant && (
        <AccessDialog
          running={p.running}
          name={name}
          displayName={s.display_name}
          agent={grant.agent}
          form={p.access_form}
          {...(grant.row ? { access: grant.row.access as unknown } : {})}
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
        open={uninstalling}
        onOpenChange={(o) => {
          if (!o) setUninstalling(false);
        }}
        tone="danger"
        title={'Uninstall ' + s.display_name + '?'}
        description={uninstallWords(
          s.display_name,
          removed,
          p.access.map((r) => r.agent.name),
        )}
        footer={
          <>
            <DialogClose>
              <Button variant="secondary">Cancel</Button>
            </DialogClose>
            <Button variant="danger" onClick={() => void uninstall(removed)} disabled={action.busy}>
              Uninstall
            </Button>
          </>
        }
      >
        {action.error && <ErrorBanner error={action.error} />}
      </Dialog>
    </div>
  );
}
