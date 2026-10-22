import { useEffect, useState } from 'react';
import { Link, useHref, useLocation, useNavigate } from 'react-router';
import { apiPostForm } from '../../api/client';
import type { IntegrationSummary, IntegrationUploadRefused, IntegrationVersion, Integrations } from '../../api/types.gen';
import { Badge, Banner, Button, Dialog, DialogClose, Icon, ListRow, Text } from '../../ds';
import { pickFiles } from '../../lib/pickFiles';
import { INTEGRATIONS_API, StatusBadge, agentsLabel, integrationPath, uploadRefusalOf, versionPath } from './integrationParts';
import { ErrorBanner, None, RowList, RowSkeletons, SectionHead } from './parts';
import type { SectionProps } from './sections';
import { useAction, useResource } from './useResource';
import '../../styles/integrations.css';

/** "2 installed · 1 waiting": waiting counts the integrations with a version for you to approve. */
export function integrationsMeta(list: readonly IntegrationSummary[]): string {
  const waiting = list.filter((i) => i.pending_version).length;
  return `${list.length} installed` + (waiting > 0 ? ` · ${waiting} waiting` : '');
}

/** One integration: its row opens its page; a version waiting for approval links to its install screen below it. */
function IntegrationRow({ item }: { item: IntegrationSummary }) {
  const href = useHref(integrationPath(item.name));
  return (
    <div className="app-int-item">
      <ListRow
        className="app-int-row"
        lead={<Icon name="plug" size={20} />}
        title={item.display_name}
        meta={
          <>
            {item.builtin && <Badge variant="outline">Built in</Badge>}
            {item.version && <span>{'v' + item.version.replace(/^v/, '')}</span>}
            {item.version && <span aria-hidden="true">·</span>}
            <StatusBadge status={item.status} />
            <span aria-hidden="true">·</span>
            <span>{agentsLabel(item.agents)}</span>
          </>
        }
        trail={<Icon name="chevron-right" />}
        href={href}
      />
      {item.pending_version && (
        <Link className="app-int-pending" to={versionPath(item.name, item.pending_version)}>
          {'Version ' + item.pending_version + ' waits for your approval'}
          <Icon name="arrow-right" />
        </Link>
      )}
    </div>
  );
}

/** Org > Integrations: every installed integration, and the upload that adds one. */
export function IntegrationsSection({ showHead }: SectionProps) {
  const list = useResource<Integrations>(INTEGRATIONS_API);
  const action = useAction();
  const navigate = useNavigate();
  const [refused, setRefused] = useState<IntegrationUploadRefused | null>(null);
  // A browser sign-in Kivali could not tie to an integration comes back here with why, read once and taken off the
  // address.
  const location = useLocation();
  const [signInError, setSignInError] = useState(() => new URLSearchParams(location.search).get('sign_in_error'));
  useEffect(() => {
    if (new URLSearchParams(location.search).has('sign_in_error')) void navigate({ pathname: location.pathname, search: '' }, { replace: true });
  }, [location.search, location.pathname, navigate]);

  const upload = async () => {
    const [file] = await pickFiles({ multiple: false, accept: '.zip,application/zip' });
    if (!file) return;
    await action.run(async () => {
      const form = new FormData();
      form.append('file', file);
      let version: IntegrationVersion;
      try {
        version = await apiPostForm<IntegrationVersion>(INTEGRATIONS_API, form);
      } catch (err) {
        const refusal = uploadRefusalOf(err);
        if (!refusal) throw err;
        setRefused(refusal);
        return;
      }
      void navigate(versionPath(version.name, version.version));
    });
  };

  const items = list.data?.integrations;
  return (
    <>
      <SectionHead title="Integrations" show={showHead} meta={items ? integrationsMeta(items) : undefined} />
      <Text as="p" variant="caption" tone="muted">
        Services your agents can act on, each from a bundle you approve. You connect each one, then choose which agents may use it and how far.
      </Text>
      <ErrorBanner error={list.error ?? action.error} {...(action.error ? { onDismiss: action.dismiss } : {})} />
      {signInError && (
        <Banner tone="danger" title="The sign-in did not work" onDismiss={() => setSignInError(null)}>
          {signInError}
        </Banner>
      )}
      {!items && !list.error && <RowSkeletons />}
      {items && items.length === 0 && <None>No integrations installed.</None>}
      {items && items.length > 0 && (
        <RowList>
          {items.map((item) => (
            <IntegrationRow key={item.name} item={item} />
          ))}
        </RowList>
      )}
      <div className="app-org-actions">
        <Button variant="secondary" icon={<Icon name="upload" />} onClick={() => void upload()} disabled={action.busy}>
          Upload a bundle
        </Button>
      </div>

      <Dialog
        open={refused !== null}
        onOpenChange={(o) => {
          if (!o) setRefused(null);
        }}
        title="The bundle did not pass the check"
        description={refused ? (refused.who === 'you' ? 'Fix these in the bundle, then upload it again.' : refused.who) : undefined}
        footer={
          <DialogClose>
            <Button variant="secondary">Close</Button>
          </DialogClose>
        }
      >
        {refused && (
          <ul className="app-int-findings" aria-label="What the check found">
            {refused.findings.map((f, i) => (
              <li key={i}>
                <Text variant="body">{f}</Text>
              </li>
            ))}
          </ul>
        )}
      </Dialog>
    </>
  );
}
