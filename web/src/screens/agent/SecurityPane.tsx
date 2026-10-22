import { useId, useState } from 'react';
import { Link } from 'react-router';
import { apiDelete, apiPost } from '../../api/client';
import type { AgentNetwork, AgentNetworkExempt, NetworkAdd } from '../../api/types.gen';
import { Banner, Button, Dialog, DialogClose, Skeleton, Switch, Text } from '../../ds';
import { AddField, HostList } from '../org/Lists';
import { useAction } from '../org/useResource';
import { useFetched } from './tabsShared';
import '../../styles/org.css';
import '../../styles/agent-tabs.css';

/** The switch's hint: what turning the limits off changes. The confirmation says why that is a risk. */
const NO_LIMITS = 'Its commands can then reach any public host, not just the listed ones, and nothing counts what it sends.';

/** The confirmation: why the limits matter, then what the agent can do without them. */
const noLimitsWarning = (name: string) =>
  `Something an agent reads, such as a web page, can trick it into sending your data to someone else. Without limits, ${name} can send anything it can read to any public host.`;

/**
 * About > Security: what this agent can reach beyond the team's defaults. The team's host list is read here and
 * edited on the Org page; the agent's own additions are edited here, and one switch lifts every network limit
 * for this agent, after a confirmation.
 */
export function SecurityPane({ slug, name }: { slug: string; name: string }) {
  const path = '/api/v1/agents/' + encodeURIComponent(slug) + '/network';
  const { data, error, loading, set } = useFetched<AgentNetwork>(path);
  const action = useAction();
  const [confirming, setConfirming] = useState(false);
  const headingId = useId();

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
      <Banner tone="danger" title={error?.message ?? 'This agent’s network settings could not be read.'}>
        {error?.who}
      </Banner>
    );
  }

  const add = (host: string) =>
    action.run(async () => {
      const body: NetworkAdd = { host };
      set(await apiPost<AgentNetwork>(path, body));
    });
  const remove = (host: string) =>
    action.run(async () => {
      set(await apiDelete<AgentNetwork>(path + '/' + encodeURIComponent(host)));
    });
  const setExempt = (exempt: boolean) =>
    action.run(async () => {
      const body: AgentNetworkExempt = { exempt };
      set(await apiPost<AgentNetwork>(path + '/exempt', body));
    });
  // The dialog closes whichever way the request ends, so a refusal shows in the pane's banner, not behind it.
  const confirm = async () => {
    await setExempt(true);
    setConfirming(false);
  };
  const failure = action.error;

  return (
    <div className="app-about-pane">
      {data.exempt && (
        <Banner tone="warning" title="No network limits">
          This agent’s commands and fetches can send anything, of any size, anywhere. Kivali does not stop or count it.
        </Banner>
      )}
      {failure && (
        <Banner tone="danger" title={failure.message} onDismiss={action.dismiss}>
          {failure.who === 'you' ? 'Check the host, then try again.' : failure.who}
        </Banner>
      )}
      <section className="app-security-section" aria-labelledby={headingId}>
        <Text as="h2" variant="heading" id={headingId}>
          Where this agent’s commands can connect
        </Text>
        <Text as="h3" variant="label">
          For every agent
        </Text>
        <HostList hosts={data.team} empty="The team’s list is empty." />
        <Text as="p" variant="caption" tone="muted">
          <Link className="app-security-link" to="/org/network">
            Change the team’s list in Org, Network
          </Link>
        </Text>
        <Text as="h3" variant="label">
          Only for {name}
        </Text>
        <HostList
          hosts={data.own.map((host) => ({ host, permanent: false }))}
          empty="No additions."
          remove={{ busy: action.busy, onRemove: (host) => void remove(host) }}
        />
        <AddField label="Allow a host" placeholder="api.github.com" button="Allow" busy={action.busy} onAdd={add} />
        <Text as="p" variant="caption" tone="muted">
          An entry covers every name under it. Changes apply at once.
        </Text>
      </section>
      {/* The switch is the section: its label names it, so there is no second heading saying the same words. */}
      <section className="app-security-limits" aria-label="Network limits">
        <Switch
          label="Remove all network limits for this agent"
          hint={NO_LIMITS}
          checked={data.exempt}
          disabled={action.busy}
          onCheckedChange={(on) => {
            // Lifting the limits asks first; putting them back is safe, so it happens at once.
            if (on) setConfirming(true);
            else void setExempt(false);
          }}
        />
      </section>
      <Dialog
        open={confirming}
        onOpenChange={(o) => {
          if (!o) setConfirming(false);
        }}
        tone="danger"
        title={`Remove all network limits for ${name}?`}
        description={noLimitsWarning(name)}
        footer={
          <>
            <DialogClose>
              <Button variant="secondary">Cancel</Button>
            </DialogClose>
            <Button variant="danger" onClick={() => void confirm()} disabled={action.busy}>
              Remove limits
            </Button>
          </>
        }
      />
    </div>
  );
}
