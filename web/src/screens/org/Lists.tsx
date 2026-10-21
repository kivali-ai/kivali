import { Fragment, useState } from 'react';
import { Link } from 'react-router';
import { apiDelete, apiPost } from '../../api/client';
import type { Network, NetworkAdd, NetworkHost } from '../../api/types.gen';
import { Badge, Button, Icon, ListRow, Text, TextField } from '../../ds';
import { ErrorBanner, None, RowList, RowSkeletons, SectionHead } from './parts';
import type { SectionProps } from './sections';
import { useAction, useResource } from './useResource';

export interface AddFieldProps {
  label: string;
  placeholder: string;
  button: string;
  busy: boolean;
  onAdd(value: string): Promise<boolean>;
}

/** One text field and its button. It clears once the server has taken the value. */
export function AddField({ label, placeholder, button, busy, onAdd }: AddFieldProps) {
  const [value, setValue] = useState('');
  const trimmed = value.trim();
  return (
    <form
      className="app-org-add"
      onSubmit={(e) => {
        e.preventDefault();
        if (trimmed)
          void onAdd(trimmed).then((ok) => {
            if (ok) setValue('');
          });
      }}
    >
      <div className="app-org-add-field">
        <TextField label={label} placeholder={placeholder} value={value} onChange={(e) => setValue(e.target.value)} />
      </div>
      <div className="app-org-add-action">
        <Button type="submit" variant="secondary" disabled={!trimmed || busy}>
          {button}
        </Button>
      </div>
    </form>
  );
}

interface HostListProps {
  hosts: readonly NetworkHost[];
  /** Shown instead of the list when it is empty. */
  empty?: string;
  /** Without it the list is read-only. */
  remove?: { busy: boolean; onRemove(host: string): void };
}

/**
 * Allowed hosts, one per row. A permanent host is marked and never offers Remove: the server refuses that
 * removal. With `remove`, each of the others can be taken off.
 */
export function HostList({ hosts, empty, remove }: HostListProps) {
  return (
    <>
      {hosts.length === 0 && empty && <None>{empty}</None>}
      {hosts.length > 0 && (
        <RowList>
          {hosts.map((h) => (
            <ListRow
              key={h.host}
              lead={<Icon name="globe" size={20} />}
              title={<Text variant="code">{h.host}</Text>}
              {...(h.permanent
                ? {
                    meta: (
                      <>
                        <Badge variant="outline">Permanent</Badge> Kivali installs approved software from here
                      </>
                    ),
                  }
                : remove
                  ? {
                      trail: (
                        <Button variant="ghost" size="sm" aria-label={`Remove ${h.host}`} onClick={() => remove.onRemove(h.host)} disabled={remove.busy}>
                          Remove
                        </Button>
                      ),
                    }
                  : {})}
            />
          ))}
        </RowList>
      )}
    </>
  );
}

/** The agents with no network limits, each linking to the Security view of its About tab, where the switch is. */
function ExemptAgents({ agents }: { agents: Network['exempt'] }) {
  if (agents.length === 0) return null;
  return (
    <Text as="p" variant="caption" tone="muted">
      No network limits:{' '}
      {agents.map((a, i) => (
        <Fragment key={a.slug}>
          {i > 0 && ', '}
          <Link className="app-org-link" to={'/agents/' + encodeURIComponent(a.slug) + '/about?view=security'}>
            {a.name}
          </Link>
        </Fragment>
      ))}
    </Text>
  );
}

export function NetworkSection({ showHead }: SectionProps) {
  const list = useResource<Network>('/api/v1/org/network');
  const action = useAction();
  const add = (host: string) =>
    action.run(async () => {
      const body: NetworkAdd = { host };
      list.set(await apiPost<Network>('/api/v1/org/network', body));
    });
  const remove = (host: string) =>
    action.run(async () => {
      list.set(await apiDelete<Network>('/api/v1/org/network/' + encodeURIComponent(host)));
    });
  const n = list.data?.hosts.length;
  return (
    <>
      <SectionHead title="Network" show={showHead} meta={n === undefined ? undefined : `${n} ${n === 1 ? 'host' : 'hosts'} agent shells can reach · an entry covers every name under it · changes apply at once`} />
      <ErrorBanner error={list.error ?? action.error} {...(action.error ? { onDismiss: action.dismiss } : {})} />
      {!list.data && !list.error && <RowSkeletons />}
      {list.data && <HostList hosts={list.data.hosts} remove={{ busy: action.busy, onRemove: (host) => void remove(host) }} />}
      <AddField label="Allow a host" placeholder="example.com" button="Allow" busy={action.busy} onAdd={add} />
      {list.data && <ExemptAgents agents={list.data.exempt} />}
    </>
  );
}
