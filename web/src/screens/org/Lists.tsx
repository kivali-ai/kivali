import { useState } from 'react';
import { apiDelete, apiPost } from '../../api/client';
import type { Network, NetworkAdd } from '../../api/types.gen';
import { Badge, Button, Icon, ListRow, Text, TextField } from '../../ds';
import { ErrorBanner, RowList, RowSkeletons, SectionHead } from './parts';
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
      <SectionHead title="Network" show={showHead} meta={n === undefined ? undefined : `${n} ${n === 1 ? 'host' : 'hosts'} agent shells can reach · changes apply at once`} />
      <ErrorBanner error={list.error ?? action.error} {...(action.error ? { onDismiss: action.dismiss } : {})} />
      {!list.data && !list.error && <RowSkeletons />}
      {list.data && (
        <RowList>
          {list.data.hosts.map((h) => (
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
                : {
                    trail: (
                      <Button variant="ghost" size="sm" aria-label={`Remove ${h.host}`} onClick={() => void remove(h.host)} disabled={action.busy}>
                        Remove
                      </Button>
                    ),
                  })}
            />
          ))}
        </RowList>
      )}
      <AddField label="Allow a host" placeholder="api.example.com or *.example.com" button="Allow" busy={action.busy} onAdd={add} />
    </>
  );
}
