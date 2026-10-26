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
  requirementsSummary,
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

/**
 * What Test connection last found, under the connection's form: whether it passed, when, and the integration's own
 * words; undefined until it has run (Save connection and Sign out clear it). The Requirements row that keeps the same
 * result is closed while every requirement is met, so this line is where pressing the button shows.
 */
export function testLine(c: ConnectionState | undefined, now: number): { text: string; passed: boolean } | undefined {
  if (!c || c.verified === undefined || !c.verified_at) return undefined;
  const at = whenLabel(c.verified_at, now);
  const words = c.verify_message?.trim() ?? '';
  if (c.verified) return { text: 'Connection test passed ' + at + (words ? '. ' + words : '.'), passed: true };
  return { text: 'Connection test failed ' + at + (words ? ': ' + words : '.'), passed: false };
}

/** The last connection test's line; nothing until one has run. */
function TestLine({ connection, now }: { connection: ConnectionState | undefined; now: number }) {
  const line = testLine(connection, now);
  if (!line) return null;
  return (
    <Text as="p" variant="caption" tone={line.passed ? 'success' : 'danger'}>
      {line.text}
    </Text>
  );
}

/**
 * The Connection block's line beside its heading, read without opening it: a sign-in that needs the owner, in the
 * danger tone, else since when it is connected. Empty with no connection.
 */
export function connectionSummary(c: ConnectionState | undefined, now: number): { text: string; danger: boolean } {
  if (!c) return { text: '', danger: false };
  const signIn = signInLine(c, now);
  if (signIn?.danger) return signIn;
  return { text: 'Connected since ' + whenLabel(c.created_at, now), danger: false };
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
 * A connected integration's one connection: its form, one field per entry the bundle asks for (a stored secret shows
 * only that it is stored), with Save connection; made by signing in, when the owner signed in; Test connection where
 * there is one, with what the last test found under the form; and, made by signing in, the Sign in block. A connection
 * its own setup made has no form. Since when it is connected is beside the block's heading; how it stands (secrets,
 * sign-in, the last test) is in Requirements too.
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
      <SignInLine connection={connection} now={now} />
      <TestLine connection={connection} now={now} />
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

/** Who loses access when the connection goes, for a confirm: "No agent has access. ", "Dana and Lee lose their access. ". */
function accessGoes(agents: readonly string[]): string {
  return agents.length === 0 ? 'No agent has access. ' : namesInWords(agents) + (agents.length === 1 ? ' loses its access. ' : ' lose their access. ');
}

/**
 * What Disconnect does, for its dialog: the service's side undone where it can be, the connection and its secrets
 * forgotten, whose access goes, and that the integration stays, ready to connect again.
 */
export function disconnectWords(display: string, agents: readonly string[]): string {
  return 'Kivali undoes the installation on the service where it can, then forgets the connection and its secrets. ' + accessGoes(agents) + display + ' stays installed, ready to connect again.';
}

/**
 * What Uninstall does, for its dialog: with a connection, what Disconnect does first; then that the integration goes
 * whole, with every version and its log.
 */
export function uninstallWords(display: string, connected: boolean, agents: readonly string[]): string {
  const lead = connected ? 'Kivali undoes the installation on the service where it can, then forgets the connection and its secrets. ' + accessGoes(agents) : '';
  return lead + display + ' goes with every version and its log; uploading a bundle installs it again.';
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

/** "6 actions", "1 action", "No actions": what the integration gives agents, beside the block's heading. */
export function countLabel(n: number, one: string, many: string): string {
  if (n === 0) return 'No ' + many;
  return n + ' ' + (n === 1 ? one : many);
}

/**
 * An integration's page under Org, drawn from its lifecycle state, with what the owner most often comes for first.
 * Installed (no connection): the Connect block, whose flow makes the connection, with the vendor-side notes and, under
 * it, what connecting does. Connected or disabled: its requirements (open while one is not met, each unmet one with
 * its Fix; a line when all are met), which agents have access and how far, and its connection, closed, with its form,
 * the notes and Disconnect. Every state ends with what agents can do, its software, its versions, recent calls and
 * recent logs, each closed. Uninstall, for a bundle, is in the header. Every change answers with the whole page.
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
  const [disconnecting, setDisconnecting] = useState(false);
  const [grant, setGrant] = useState<Grant | null>(null);
  const [rollingBack, setRollingBack] = useState<string | null>(null);
  const [fixing, setFixing] = useState<Fixing | null>(null);
  const [pick, setPick] = useState('');
  // Bumped after a save so the connection form starts again from what is stored.
  const [formSeq, setFormSeq] = useState(0);
  // The Requirements and Connection blocks as the owner left them; null while they follow the page.
  const [reqOpen, setReqOpen] = useState<boolean | null>(null);
  const [connOpen, setConnOpen] = useState<boolean | null>(null);
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
  const rereadAudit = () => void apiGet<IntegrationAudit>(api + '/audit').then(audit.set, () => {});
  // A flow that finished, or a sign-out, changes the page and adds a row to the audit, which are read again.
  const flowDone = () =>
    action.run(async () => {
      page.set(await apiGet<IntegrationPageData>(api));
      audit.set(await apiGet<IntegrationAudit>(api + '/audit'));
      setFixing(null);
    });
  const signedOut = (next: IntegrationPageData) => {
    page.set(next);
    rereadAudit();
  };
  const revoke = (slug: string) => void mutate(() => apiDelete<IntegrationPageData>(api + '/access/' + encodeURIComponent(slug)));
  // Disconnect answers with the page, installed again with no connection, and adds a row to the audit.
  const disconnect = () =>
    action.run(async () => {
      page.set(await apiDelete<IntegrationPageData>(api + '/connection'));
      setDisconnecting(false);
      setFixing(null);
      setFormSeq((n) => n + 1);
      rereadAudit();
    });
  // Uninstall takes the integration off the team, back to the list.
  const uninstall = () =>
    action.run(async () => {
      await apiDelete(api);
      setUninstalling(false);
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
  const state = s.state;
  // Holds a connection this version takes: connected or disabled.
  const configured = state === 'connected' || state === 'disabled';
  const pending = s.pending_version;
  const invalid = invalidAccessLine(p.access.filter((r) => r.invalid).length);
  const chosen = p.agents_without.some((a) => a.slug === pick) ? pick : (p.agents_without[0]?.slug ?? '');
  const signIn = p.connection_method === 'device' || p.connection_method === 'browser';
  const unmet = p.requirements.filter((r) => !r.met).length;
  // The flow a Fix runs: one the owner just started, or one under way (or failed) that the page carries, such as a
  // Go step's service sending the browser back here.
  const fixFlow: Fixing | null =
    fixing ??
    (configured && p.flow && SHOWN_FLOW.has(p.flow.state)
      ? { flow: p.flow.flow, title: p.requirements.find((r) => r.fix === p.flow?.flow)?.title ?? 'Fix', seq: 0 }
      : configured && returned
        ? signIn
          ? { flow: 'sign_in', title: p.requirements.find((r) => r.fix === 'sign_in')?.title ?? 'Fix', seq: 0 }
          : { flow: 'setup', title: 'Fix', seq: 0 }
        : null);
  // Where connecting signs in, for a sign-in connection: every way's host, as the install screen names them.
  const connectNote = signIn ? signsInAt(Object.keys(p.sign_in_at ?? {}), p.sign_in_at) : undefined;
  // Where signing in again sends the owner, for the sign-in row's Fix.
  const fixNote = fixFlow?.flow === 'sign_in' ? signsInAt((p.connection?.sign_in?.ways ?? []).slice(0, 1), p.sign_in_at) : undefined;
  // Requirements open while one needs the owner, or a Fix runs; the connection stays closed until asked for.
  const requirementsOpen = reqOpen ?? (unmet > 0 || fixFlow !== null);
  const connectionOpen = connOpen ?? false;
  // The declarations from the manifest, read once: what to do on the vendor's side (notes, the redirect address to
  // register) and what connecting does (setup's hosts, fields and keeps; the scopes a sign-in asks for).
  const notes = p.setup_notes_md.trim() !== '' || p.sign_in_redirect_url;
  const facts = p.setup || p.scopes.length > 0;
  const accessAgents = p.access.map((r) => r.agent.name);

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

  // What the vendor's side needs first: the manifest's notes and the redirect address to register with the service.
  const vendorNotes = notes && (
    <>
      <Markdown markdown={p.setup_notes_md} />
      {p.sign_in_redirect_url && <CopyAddress label="Redirect address" hint="Register this address as the app’s callback URL." url={p.sign_in_redirect_url} />}
    </>
  );
  // What connecting does, from the manifest: on the Connect block, closed, and under the connection once made.
  const connectingDoes = facts && (
    <Block label="What connecting does" level="h3" collapsible>
      {p.setup && <SetupFacts setup={p.setup} integration={s.display_name} />}
      <ScopesLine scopes={p.scopes} />
    </Block>
  );
  const disconnectButton = p.can_disconnect && (
    <div className="app-org-actions">
      <Button variant="ghost" disabled={action.busy} onClick={() => setDisconnecting(true)}>
        Disconnect
      </Button>
    </div>
  );

  return (
    <div className="app-int">
      <header className="app-int-head">
        <PageTitle>{s.display_name}</PageTitle>
        <div className="app-int-head-row">
          <StateBadge state={state} />
          {state === 'connected' && <NeedsBadge needs={s.needs} />}
          {s.builtin && <Badge variant="outline">Built in</Badge>}
          <Text variant="caption" tone="muted">
            {s.version ? 'Version ' + s.version : 'No version approved yet'}
          </Text>
          <span className="app-int-spacer" />
          {configured && <Switch label="Enabled" checked={s.enabled} disabled={action.busy} onCheckedChange={(on) => void toggle(on)} />}
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
        {state === 'disabled' && (
          <Text as="p" variant="caption" tone="muted">
            Disabled: agents cannot use it. Its connection and every agent’s access are kept for when you enable it.
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
        <Block label="Connect" variant="heading">
          <Text as="p" variant="body" tone="muted">
            Approve a version first.
          </Text>
        </Block>
      )}

      {state === 'installed' && (
        <Block label="Connect" variant="heading">
          {/* An integration whose connection an earlier version made, which this version's setup does not take, says
              so here: connecting again is the Fix, and Disconnect drops it instead. */}
          {p.requirements
            .filter((r) => !r.met)
            .map((r) => (
              <Banner key={r.id} tone="warning">
                {r.words}
              </Banner>
            ))}
          {vendorNotes}
          <SetupFlowPanel
            api={api}
            integration={s.display_name}
            label="Setup"
            startLabel="Connect"
            busy={action.busy}
            returned={returned}
            {...(connectNote ? { note: connectNote } : {})}
            onDone={flowDone}
          />
          {disconnectButton}
          {connectingDoes}
        </Block>
      )}

      {configured && (
        <Block label="Requirements" variant="heading" collapsible open={requirementsOpen} onOpenChange={setReqOpen} summary={requirementsSummary(unmet)}>
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

      {configured && (
        <Block label="Access" variant="heading" summary={agentsLabel(p.access.length)}>
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

      {configured && (
        <Block
          label="Connection"
          variant="heading"
          collapsible
          open={connectionOpen}
          onOpenChange={setConnOpen}
          summary={connectionSummary(p.connection, now).text}
          summaryTone={connectionSummary(p.connection, now).danger ? 'danger' : 'muted'}
        >
          <ConnectionForm
            key={formSeq}
            page={p}
            busy={action.busy}
            now={now}
            onSave={save}
            {...(p.connection_test ? { onVerify: verify } : {})}
            onPage={signedOut}
          />
          {notes && (
            <Block label="Setup notes" level="h3">
              {vendorNotes}
            </Block>
          )}
          {connectingDoes}
          {disconnectButton}
        </Block>
      )}

      <Block label="Actions" variant="heading" collapsible summary={countLabel(p.actions.length, 'action', 'actions')}>
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

      {/* A bundle's; a built-in installs none. */}
      {p.packages.length > 0 && (
        <Block label="Software" variant="heading" collapsible summary={countLabel(p.packages.length, 'package', 'packages')}>
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

      <Block label="Versions" variant="heading" collapsible summary={countLabel(p.versions.length, 'version', 'versions')}>
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
        open={disconnecting}
        onOpenChange={(o) => {
          if (!o) setDisconnecting(false);
        }}
        tone="danger"
        title={'Disconnect ' + s.display_name + '?'}
        description={disconnectWords(s.display_name, accessAgents)}
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
      >
        {action.error && <ErrorBanner error={action.error} />}
      </Dialog>
      <Dialog
        open={uninstalling}
        onOpenChange={(o) => {
          if (!o) setUninstalling(false);
        }}
        tone="danger"
        title={'Uninstall ' + s.display_name + '?'}
        description={uninstallWords(s.display_name, p.connection !== undefined && p.connection !== null, accessAgents)}
        footer={
          <>
            <DialogClose>
              <Button variant="secondary">Cancel</Button>
            </DialogClose>
            <Button variant="danger" onClick={() => void uninstall()} disabled={action.busy}>
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
