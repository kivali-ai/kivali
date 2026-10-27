import { useEffect, useState } from 'react';
import { Link, useHref, useLocation, useNavigate } from 'react-router';
import { apiPostForm } from '../../api/client';
import type { IntegrationSummary, IntegrationUploadRefused, IntegrationVersion, Integrations } from '../../api/types.gen';
import { Banner, Button, Dialog, DialogClose, Icon, ListRow, Text } from '../../ds';
import { pickFiles } from '../../lib/pickFiles';
import { Block, INTEGRATIONS_API, NeedsBadge, StateBadge, agentsLabel, needsLabel, integrationPath, signInErrorWords, uploadRefusalOf, versionPath } from './integrationParts';
import { ErrorBanner, None, RowList, RowSkeletons, SectionHead } from './parts';
import type { SectionProps } from './sections';
import { useAction, useResource } from './useResource';
import '../../styles/integrations.css';

/**
 * "2 connected · 1 disabled · 1 needs you · 1 waiting": connected counts those connected, disabled those with a
 * connection and the switch off, needs you those connected with a requirement not met, and waiting those with a
 * version for you to approve.
 */
export function integrationsMeta(list: readonly IntegrationSummary[]): string {
  const connected = list.filter((i) => i.state === 'connected').length;
  const disabled = list.filter((i) => i.state === 'disabled').length;
  const needs = list.filter((i) => i.state === 'connected' && i.needs > 0).length;
  const waiting = list.filter((i) => i.pending_version).length;
  return `${connected} connected` + (disabled > 0 ? ` · ${disabled} disabled` : '') + (needs > 0 ? ` · ${needsLabel(needs)}` : '') + (waiting > 0 ? ` · ${waiting} waiting` : '');
}

function countLabel(n: number): string {
  return n === 1 ? '1 integration' : n + ' integrations';
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
            {item.version && <span>{'v' + item.version.replace(/^v/, '')}</span>}
            {item.version && <span aria-hidden="true">·</span>}
            <StateBadge state={item.state} />
            {item.state === 'connected' && <NeedsBadge needs={item.needs} />}
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

/**
 * Org > Integrations: every installed integration in two groups, those from a bundle (an upload, or one the Kivali
 * Developer published) with the upload that adds one, and those built in to Kivali.
 */
export function IntegrationsSection({ showHead }: SectionProps) {
  const list = useResource<Integrations>(INTEGRATIONS_API);
  const action = useAction();
  const navigate = useNavigate();
  const [refused, setRefused] = useState<IntegrationUploadRefused | null>(null);
  // A browser sign-in Kivali could not tie to an integration comes back here with a code saying why (never words), read
  // once and taken off the address.
  const location = useLocation();
  const [signInError, setSignInError] = useState(() => signInErrorWords(new URLSearchParams(location.search).get('sign_in_error')));
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
  const bundles = items?.filter((i) => !i.builtin);
  const builtins = items?.filter((i) => i.builtin);
  return (
    <>
      <SectionHead title="Integrations" show={showHead} meta={items ? integrationsMeta(items) : undefined} />
      <Text as="p" variant="caption" tone="muted">
        Services your agents can act on, each from a bundle you approve or shipped with Kivali. You connect each one, then choose which agents may use it and how far.
      </Text>
      <ErrorBanner error={list.error ?? action.error} {...(action.error ? { onDismiss: action.dismiss } : {})} />
      {signInError && (
        <Banner tone="danger" title="The sign-in did not work" onDismiss={() => setSignInError(undefined)}>
          {signInError}
        </Banner>
      )}
      {!items && !list.error && <RowSkeletons />}
      {bundles && builtins && (
        <>
          <Block label="From a bundle" level="h3" summary={bundles.length === 0 ? undefined : countLabel(bundles.length)}>
            <Text as="p" variant="caption" tone="muted">
              Uploaded by you or published by your Kivali Developer. An integration’s page has its bundle to download.
            </Text>
            {bundles.length === 0 ? (
              <None>No integrations from a bundle yet.</None>
            ) : (
              <RowList>
                {bundles.map((item) => (
                  <IntegrationRow key={item.name} item={item} />
                ))}
              </RowList>
            )}
            <div className="app-org-actions">
              <Button variant="secondary" icon={<Icon name="upload" />} onClick={() => void upload()} disabled={action.busy}>
                Upload a bundle
              </Button>
            </div>
          </Block>
          <Block label="Built in" level="h3" summary={builtins.length === 0 ? undefined : countLabel(builtins.length)}>
            <Text as="p" variant="caption" tone="muted">
              Ship with Kivali, and its updates bring their new versions.
            </Text>
            {builtins.length === 0 ? (
              <None>None in this version of Kivali.</None>
            ) : (
              <RowList>
                {builtins.map((item) => (
                  <IntegrationRow key={item.name} item={item} />
                ))}
              </RowList>
            )}
          </Block>
        </>
      )}

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
