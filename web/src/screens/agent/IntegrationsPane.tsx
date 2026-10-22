import type { AgentIntegration, AgentIntegrations } from '../../api/types.gen';
import { Banner, Icon, ListRow, Skeleton, Text } from '../../ds';
import { StatusBadge, integrationPath } from '../org/integrationParts';
import { useAgentPage } from './AgentPage';
import { useAppHref, useFetched } from './tabsShared';
import '../../styles/agent-tabs.css';
import '../../styles/integrations.css';

/** The row's name: "Git (Company GitLab)" for one of several connections, the integration's name otherwise. */
export function rowTitle(i: Pick<AgentIntegration, 'display_name' | 'connection_label'>): string {
  return i.connection_label ? i.display_name + ' (' + i.connection_label + ')' : i.display_name;
}

function IntegrationList({ items, connected, href }: { items: readonly AgentIntegration[]; connected: boolean; href(to: string): string }) {
  return (
    <div className="app-tabs-list">
      <div className="app-tabs-rows">
        {items.map((i) => (
          <ListRow
            key={i.name}
            lead={<Icon name="plug" size={20} />}
            title={rowTitle(i)}
            meta={
              <>
                <StatusBadge status={i.status} />
                {connected && i.summary && <span>{i.summary}</span>}
                {connected && i.invalid && (
                  <Text variant="caption" tone="danger">
                    {'Invalid: ' + i.invalid}
                  </Text>
                )}
              </>
            }
            trail={<Icon name="chevron-right" />}
            href={href(integrationPath(i.name))}
          />
        ))}
      </div>
    </div>
  );
}

function None({ children }: { children: string }) {
  return (
    <div className="app-tabs-list">
      <div className="app-int-none">
        <Text variant="caption" tone="muted">
          {children}
        </Text>
      </div>
    </div>
  );
}

/**
 * The agent's Integrations tab: what it is connected to and how far its access goes, then every other installed
 * integration. Access is changed on each integration's page under Org; every row opens it.
 */
export function IntegrationsPane() {
  const { slug, name } = useAgentPage();
  const href = useAppHref();
  const { data, error, loading } = useFetched<AgentIntegrations>('/api/v1/agents/' + encodeURIComponent(slug) + '/integrations');

  if (loading) {
    return (
      <div className="app-about-pane" aria-busy="true">
        <Skeleton height={56} />
        <Skeleton height={56} />
      </div>
    );
  }
  if (!data) {
    return (
      <Banner tone="danger" title={error?.message ?? 'The integrations could not be read.'}>
        {error?.who}
      </Banner>
    );
  }

  if (data.connected.length === 0 && data.available.length === 0) {
    return (
      <div className="app-about-pane">
        <None>No integrations installed.</None>
      </div>
    );
  }

  return (
    <div className="app-about-pane">
      <Text as="p" variant="label" tone="muted">
        {'What ' + name + ' can act on, and how far. You change access on each integration’s page in Org.'}
      </Text>
      <section className="app-int-group" aria-label={'Connected to ' + name}>
        <Text as="h2" variant="label">
          {'Connected to ' + name}
        </Text>
        {data.connected.length === 0 ? <None>No integrations connected.</None> : <IntegrationList items={data.connected} connected href={href} />}
      </section>
      <section className="app-int-group" aria-label="Not connected">
        <Text as="h2" variant="label">
          Not connected
        </Text>
        {data.available.length === 0 ? <None>Every installed integration is connected.</None> : <IntegrationList items={data.available} connected={false} href={href} />}
      </section>
    </div>
  );
}
