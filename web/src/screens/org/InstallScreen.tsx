import { useState } from 'react';
import { useNavigate, useParams } from 'react-router';
import { apiPost } from '../../api/client';
import type { IntegrationVersion } from '../../api/types.gen';
import { useFrameChrome } from '../../app/chrome';
import { PageTitle } from '../../app/PageTitle';
import { Badge, Banner, Button, Dialog, DialogClose, Skeleton, Text } from '../../ds';
import { Block, Markdown, VersionBadge, actionWords, fieldKindWords, integrationApi, integrationPath, whenLabel } from './integrationParts';
import { ErrorBanner, formatBytes } from './parts';
import { useAction, useResource } from './useResource';
import '../../styles/org.css';
import '../../styles/integrations.css';

/** "12 files, 180 KB" */
export function bundleLine(files: number, bytes: number): string {
  return files + (files === 1 ? ' file, ' : ' files, ') + formatBytes(bytes);
}

/**
 * A version's install screen: what it is, who built it, every host it reaches, every secret its connection
 * holds, every action and what the access form asks. Approving installs it; nothing in it runs before.
 */
export function InstallScreen() {
  const { name = '', version = '' } = useParams();
  const navigate = useNavigate();
  const path = integrationApi(name) + '/versions/' + encodeURIComponent(version);
  const res = useResource<IntegrationVersion>(path);
  const action = useAction();
  const [rejecting, setRejecting] = useState(false);
  const [now] = useState(() => Date.now());
  const v = res.data;

  // Until a version is approved the integration's page has nothing to show, so the way back is the list.
  const installed = v !== null && (v.current || Boolean(v.current_version));
  useFrameChrome({
    title: v ? v.display_name + ' ' + v.version : 'Integration',
    back: v && installed ? { to: integrationPath(name), label: v.display_name } : { to: '/org/integrations', label: 'Integrations' },
  });

  const approve = () =>
    action.run(async () => {
      await apiPost<IntegrationVersion>(path + '/approve');
      void navigate(integrationPath(name));
    });
  const reject = () =>
    action.run(async () => {
      res.set(await apiPost<IntegrationVersion>(path + '/reject'));
      setRejecting(false);
    });

  if (!v) {
    return (
      <div className="app-int" aria-busy={res.error ? undefined : true}>
        <ErrorBanner error={res.error} />
        {!res.error && (
          <>
            <Skeleton width="40%" height={28} />
            <Skeleton height={96} />
            <Skeleton height={160} />
          </>
        )}
      </div>
    );
  }

  const fields = v.access_form?.fields ?? [];
  return (
    <div className="app-int">
      <header className="app-int-head">
        <div className="app-int-head-row">
          <VersionBadge state={v.state} />
        </div>
        <PageTitle>{v.display_name + ' ' + v.version}</PageTitle>
      </header>

      <ErrorBanner error={action.error} onDismiss={action.dismiss} />

      <Block label="What it is">
        <Text as="p" variant="body">
          {v.description || 'The bundle gives no description.'}
        </Text>
      </Block>

      <Block label="Who built it">
        <Text as="p" variant="body">
          {v.author || 'The bundle names no author.'}
        </Text>
        <Text as="p" variant="caption" tone="muted">
          {'Published by ' + v.published_by.name + (whenLabel(v.published_at, now) ? ' · ' + whenLabel(v.published_at, now) : '')}
        </Text>
      </Block>

      <Block label="Version">
        <Text as="p" variant="body">
          {v.version}
        </Text>
        {v.current_version && (
          <Text as="p" variant="caption" tone="muted">
            {'Replaces version ' + v.current_version}
          </Text>
        )}
        {v.current && (
          <Text as="p" variant="caption" tone="muted">
            The version in use now
          </Text>
        )}
      </Block>

      <Block label="Version notes">
        {v.notes_md.trim() ? (
          <Markdown markdown={v.notes_md} />
        ) : (
          <Text as="p" variant="caption" tone="muted">
            No notes.
          </Text>
        )}
      </Block>

      <Block label="Reaches these hosts">
        {v.hosts.length === 0 ? (
          <Text as="p" variant="caption" tone="muted">
            No hosts.
          </Text>
        ) : (
          <ul className="app-int-facts">
            {v.hosts.map((h) => (
              <li key={h}>
                <Text variant="code">{h}</Text>
              </li>
            ))}
          </ul>
        )}
      </Block>

      <Block label="The connection needs">
        {v.connection_fields.length === 0 ? (
          <Text as="p" variant="caption" tone="muted">
            Nothing to enter.
          </Text>
        ) : (
          <ul className="app-int-facts">
            {v.connection_fields.map((f) => (
              <li key={f.key} className="app-int-fact">
                <span className="app-int-fact-title">
                  <Text variant="body">{f.title}</Text>
                  {f.sensitive && <Badge variant="outline">Secret</Badge>}
                </span>
                <Text variant="caption" tone="muted">
                  {[f.required ? 'Required' : 'Optional', f.description].filter(Boolean).join(' · ')}
                </Text>
              </li>
            ))}
          </ul>
        )}
      </Block>

      <Block label="Actions">
        {v.actions.length === 0 ? (
          <Text as="p" variant="caption" tone="muted">
            No actions.
          </Text>
        ) : (
          <ul className="app-int-facts">
            {v.actions.map((a) => (
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

      <Block label="Access form asks">
        {fields.length === 0 ? (
          <Text as="p" variant="body">
            Access is all or nothing.
          </Text>
        ) : (
          <ul className="app-int-facts">
            {fields.map((f) => (
              <li key={f.key} className="app-int-fact">
                <Text variant="body">{f.title}</Text>
                <Text variant="caption" tone="muted">
                  {[fieldKindWords(f, v.display_name), f.required ? 'required' : '', f.description].filter(Boolean).join(' · ')}
                </Text>
              </li>
            ))}
          </ul>
        )}
      </Block>

      <Block label="Bundle">
        <Text as="p" variant="body">
          {bundleLine(v.file_count, v.bundle_bytes)}
        </Text>
      </Block>

      {v.findings.length > 0 && (
        <Block label="The check noted">
          <ul className="app-int-facts">
            {v.findings.map((f, i) => (
              <li key={i}>
                <Text variant="body">{f}</Text>
              </li>
            ))}
          </ul>
        </Block>
      )}

      <Banner tone="info">The check only reads the bundle. This screen is the review: what it reaches, what it holds, what it does.</Banner>

      {v.state === 'pending' && (
        <div className="app-int-decision">
          <Button variant="ghost" onClick={() => setRejecting(true)} disabled={action.busy}>
            Reject
          </Button>
          <Button variant="primary" onClick={() => void approve()} loading={action.busy && !rejecting} disabled={action.busy}>
            Approve and install
          </Button>
        </div>
      )}

      <Dialog
        open={rejecting}
        onOpenChange={(o) => {
          if (!o) setRejecting(false);
        }}
        tone="danger"
        title={'Reject version ' + v.version + '?'}
        description="This version never runs. The version in use, if there is one, stays as it is."
        footer={
          <>
            <DialogClose>
              <Button variant="secondary">Cancel</Button>
            </DialogClose>
            <Button variant="danger" onClick={() => void reject()} disabled={action.busy}>
              Reject
            </Button>
          </>
        }
      >
        {action.error && <ErrorBanner error={action.error} />}
      </Dialog>
    </div>
  );
}
