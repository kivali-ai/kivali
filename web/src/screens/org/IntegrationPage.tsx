import { useState } from 'react';
import { useHref, useNavigate, useParams } from 'react-router';
import { apiDelete, apiPost, apiPut } from '../../api/client';
import type { AccessRow, ConnectionField, ConnectionState, ConnectionWrite, IntegrationAudit, IntegrationAuditRow, IntegrationPage as IntegrationPageData, IntegrationVersionSummary, PersonRef } from '../../api/types.gen';
import { useFrameChrome } from '../../app/chrome';
import { PageTitle } from '../../app/PageTitle';
import { Badge, Banner, Button, Dialog, DialogClose, Icon, ListRow, Select, Skeleton, Switch, Table, Text, TextField } from '../../ds';
import type { TableColumn } from '../../ds';
import { AccessDialog } from './AccessDialog';
import { Block, StatusBadge, VersionBadge, actionWords, durationLabel, integrationApi, versionPath, whenLabel } from './integrationParts';
import { ErrorBanner, None, RowList } from './parts';
import { useAction, useResource } from './useResource';
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

/** The lines under the connection form: since when it is connected and what Test connection last found. */
function connectionLines(c: ConnectionState | undefined, now: number): string[] {
  if (!c) return ['Not connected.'];
  const lines = ['Connected since ' + whenLabel(c.created_at, now)];
  if (c.verified_at) {
    const said = c.verify_message ?? '';
    lines.push(c.verified ? 'Verified ' + whenLabel(c.verified_at, now) + (said ? ': ' + said : '') : 'Could not verify' + (said ? ': ' + said : ''));
  }
  return lines;
}

interface ConnectionFormProps {
  page: IntegrationPageData;
  busy: boolean;
  now: number;
  onSave(body: ConnectionWrite): Promise<boolean>;
  onVerify(): void;
  onDisconnect(): void;
}

/** The connection: one field per entry the bundle asks for. A stored secret shows only that it is stored. */
function ConnectionForm({ page, busy, now, onSave, onVerify, onDisconnect }: ConnectionFormProps) {
  const fields = page.connection_fields;
  const [values, setValues] = useState(() => startingValues(fields));
  const connection = page.connection;
  return (
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
      </div>
      <div className="app-org-actions">
        <Button type="submit" variant="primary" disabled={busy}>
          Save connection
        </Button>
        <Button type="button" variant="secondary" disabled={busy || !connection} onClick={onVerify}>
          Test connection
        </Button>
        <Button type="button" variant="ghost" disabled={busy || !connection} onClick={onDisconnect}>
          Disconnect
        </Button>
      </div>
    </form>
  );
}

function VersionRow({ name, v, now }: { name: string; v: IntegrationVersionSummary; now: number }) {
  const href = useHref(versionPath(name, v.version));
  const notes = v.notes_md.trim().split('\n')[0] ?? '';
  return (
    <ListRow
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
}

/** A call's outcome in words: allowed, failed with what went wrong, or refused with why. */
export function auditResult(row: Pick<IntegrationAuditRow, 'allowed' | 'reason'>): string {
  if (!row.allowed) return row.reason ? 'Refused: ' + row.reason : 'Refused';
  return row.reason ? 'Failed: ' + row.reason : 'Allowed';
}

interface Grant {
  agent: PersonRef;
  row?: AccessRow;
}

/**
 * An integration's page under Org: whether it is on, its connection, which agents have access and how far, its
 * actions, its versions and its latest calls. Every change answers with the whole page.
 */
export function IntegrationPage() {
  const { name = '' } = useParams();
  const navigate = useNavigate();
  const api = integrationApi(name);
  const page = useResource<IntegrationPageData>(api);
  const audit = useResource<IntegrationAudit>(api + '/audit');
  const action = useAction();
  const [now] = useState(() => Date.now());
  const [removing, setRemoving] = useState(false);
  const [disconnecting, setDisconnecting] = useState(false);
  const [grant, setGrant] = useState<Grant | null>(null);
  const [pick, setPick] = useState('');
  // Bumped after a save so the connection form starts again from what is stored.
  const [formSeq, setFormSeq] = useState(0);
  const p = page.data;

  useFrameChrome({ title: p?.summary.display_name ?? 'Integration', back: { to: '/org/integrations', label: 'Integrations' } });

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
  const revoke = (slug: string) => void mutate(() => apiDelete<IntegrationPageData>(api + '/access/' + encodeURIComponent(slug)));
  const remove = () =>
    action.run(async () => {
      await apiDelete(api);
      void navigate('/org/integrations');
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
  const chosen = p.agents_without.some((a) => a.slug === pick) ? pick : (p.agents_without[0]?.slug ?? '');

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
    { key: 'action', label: 'Action', render: (r) => <span className="app-int-nowrap">{actionWords(r.action)}</span> },
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

      <Block label="Connection" variant="heading">
        {s.version ? (
          <ConnectionForm
            key={formSeq}
            page={p}
            busy={action.busy}
            now={now}
            onSave={save}
            onVerify={verify}
            onDisconnect={() => setDisconnecting(true)}
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
              <VersionRow key={v.version} name={name} v={v} now={now} />
            ))}
          </RowList>
        )}
      </Block>

      <Block label="Latest calls" variant="heading">
        <ErrorBanner error={audit.error} />
        {!audit.data && !audit.error && <Skeleton height={64} />}
        {audit.data && audit.data.rows.length === 0 && <None>No calls yet.</None>}
        {audit.data && audit.data.rows.length > 0 && <Table dense columns={auditColumns} rows={audit.data.rows.map((r, i) => ({ ...r, id: i }))} />}
      </Block>

      {grant && (
        <AccessDialog
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

      <Dialog
        open={disconnecting}
        onOpenChange={(o) => {
          if (!o) setDisconnecting(false);
        }}
        tone="danger"
        title={'Disconnect ' + s.display_name + '?'}
        description="Its stored connection is deleted, and agents cannot use it until you connect it again. Their access stays."
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
