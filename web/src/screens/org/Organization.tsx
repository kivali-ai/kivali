import { useEffect, useState } from 'react';
import { apiDelete, apiPostForm, apiPut } from '../../api/client';
import type { Org, OrgPut } from '../../api/types.gen';
import { Button, Skeleton, TextField } from '../../ds';
import { useOrg } from '../../state/OrgProvider';
import { LogoField } from './LogoField';
import { ErrorBanner, SectionHead } from './parts';
import type { SectionProps } from './sections';
import { useAction, useResource } from './useResource';

const PATH = '/api/v1/org';

export function OrganizationSection({ phone, showHead }: SectionProps) {
  const org = useResource<Org>(PATH);
  const { refreshMe } = useOrg();
  const action = useAction();
  const [name, setName] = useState('');
  const [ownerName, setOwnerName] = useState('');
  const saved = org.data?.name;
  const savedOwner = org.data?.owner_name;
  useEffect(() => {
    if (saved !== undefined) setName(saved);
  }, [saved]);
  useEffect(() => {
    if (savedOwner !== undefined) setOwnerName(savedOwner);
  }, [savedOwner]);

  if (!org.data) {
    return (
      <>
        <SectionHead title="Organization" show={showHead} meta="Name and mark" />
        <ErrorBanner error={org.error} />
        {!org.error && <Skeleton height={64} />}
      </>
    );
  }
  const data = org.data;
  // A name may stay empty when it already was (a team with none), so what the agents call you still saves.
  const nameOK = name.trim() !== '' || data.name === '';
  const changed = nameOK && (name.trim() !== data.name || ownerName.trim() !== data.owner_name);
  const logoSrc = data.has_logo ? data.logo_url : undefined;

  const save = () =>
    action.run(async () => {
      // owner_name only when it changed: sending it marks the name as chosen, which stops boot from seeding one.
      const body: OrgPut = { name: name.trim() };
      if (ownerName.trim() !== data.owner_name) body.owner_name = ownerName.trim();
      org.set(await apiPut<Org>(PATH, body));
      await refreshMe();
    });
  const upload = (files: File[]) => {
    const file = files[0];
    if (!file) return;
    void action.run(async () => {
      const form = new FormData();
      form.append('logo', file);
      org.set(await apiPostForm<Org>(PATH + '/logo', form));
      await refreshMe();
    });
  };
  const remove = () =>
    action.run(async () => {
      org.set(await apiDelete<Org>(PATH + '/logo'));
      await refreshMe();
    });

  return (
    <>
      <SectionHead title="Organization" show={showHead} meta="Name and mark" />
      <ErrorBanner error={action.error} onDismiss={action.dismiss} />
      <div className={phone ? 'app-org-identity is-phone' : 'app-org-identity'}>
        <form
          className="app-org-name"
          onSubmit={(e) => {
            e.preventDefault();
            if (changed) void save();
          }}
        >
          <TextField label="Name" value={name} onChange={(e) => setName(e.target.value)} hint="Shown in the sidebar and on sign-in." />
          <TextField
            label="What your agents call you"
            value={ownerName}
            maxLength={40}
            onChange={(e) => setOwnerName(e.target.value)}
            hint="For example Jane, Mom or CEO. Every agent uses it from its next turn."
          />
          <div>
            <Button type="submit" variant="primary" disabled={!changed || action.busy}>
              Save
            </Button>
          </div>
        </form>
        <LogoField
          name={data.name}
          src={logoSrc}
          accept="image/png"
          hint="PNG, square, at least 64 px a side."
          disabled={action.busy}
          onFiles={upload}
          onRemove={() => void remove()}
        />
      </div>
    </>
  );
}
