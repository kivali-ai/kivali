import { apiDelete, apiPost } from '../../api/client';
import type { AgentSoftware, SoftwareAdd } from '../../api/types.gen';
import { Banner, Skeleton, Text } from '../../ds';
import { PackageList } from '../org/Software';
import { useAction } from '../org/useResource';
import { useFetched } from './tabsShared';
import '../../styles/org.css';
import '../../styles/agent-tabs.css';

/**
 * About > Software: what the agent's shell has installed beyond what Kivali includes. The org-wide list is read
 * here and edited on the Org page; the agent's own list is edited here.
 */
export function SoftwarePane({ slug, name }: { slug: string; name: string }) {
  const path = '/api/v1/agents/' + encodeURIComponent(slug) + '/software';
  const { data, error, loading, set } = useFetched<AgentSoftware>(path);
  const action = useAction();

  if (loading) {
    return (
      <div className="app-about-pane" aria-busy="true">
        <Skeleton height={56} />
        <Skeleton height={120} />
      </div>
    );
  }
  if (!data) {
    return (
      <Banner tone="danger" title={error?.message ?? 'This software list could not be read.'}>
        {error?.who}
      </Banner>
    );
  }

  const add = (packages: string[]) =>
    action.run(async () => {
      const body: SoftwareAdd = { packages };
      set(await apiPost<AgentSoftware>(path, body));
    });
  const remove = (pkg: string) =>
    action.run(async () => {
      set(await apiDelete<AgentSoftware>(path + '/' + encodeURIComponent(pkg)));
    });
  const failure = action.error ?? null;

  return (
    <div className="app-about-pane">
      <Text as="p" variant="label" tone="muted">
        Software is installed when {name} starts. Changing this list restarts {name} to install it, once {name} has finished what it is doing.
      </Text>
      {failure && (
        <Banner tone="danger" title={failure.message} onDismiss={action.dismiss}>
          {failure.who === 'you' ? 'Check the names, then try again.' : failure.who}
        </Banner>
      )}
      <Text as="h3" variant="label">
        For every agent
      </Text>
      <PackageList packages={data.org} invalid={data.invalid} empty="Nothing is installed for every agent. You can add software on the Org page." />
      <Text as="h3" variant="label">
        Only for {name}
      </Text>
      <PackageList
        packages={data.own}
        invalid={data.invalid}
        empty={`Nothing extra for ${name} yet. ${name} can ask for software, and you decide.`}
        edit={{
          busy: action.busy,
          label: 'Add software',
          onAdd: add,
          onRemove: (n) => void remove(n),
        }}
      />
    </div>
  );
}
