import { useState } from 'react';
import { apiPost, apiPostForm } from '../../api/client';
import type { Setup } from '../../api/types.gen';
import { ORG_LOGO_SRC } from '../../app/orgIdentity';
import { Button, TextField } from '../../ds';
import { LogoField } from '../org/LogoField';
import { Actions, ErrorBanner, StepHead } from './parts';

export interface OrgStepProps {
  setup: Setup;
  onSetup(next: Setup): void;
  onBack(): void;
  onNext(): void;
}

/** Step 2: name and logo, both optional. The mark beside the logo drop is the live preview. */
export function OrgStep({ setup, onSetup, onBack, onNext }: OrgStepProps) {
  const [name, setName] = useState(setup.org.name);
  const [logoNonce, setLogoNonce] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  async function uploadLogo(files: File[]) {
    const logo = files[0];
    if (!logo) return;
    const form = new FormData();
    form.append('logo', logo);
    setError(null);
    try {
      onSetup(await apiPostForm<Setup>('/api/v1/setup/org/logo', form));
      setLogoNonce((n) => n + 1);
    } catch (err) {
      setError(err);
    }
  }

  async function next() {
    const trimmed = name.trim();
    setError(null);
    if (trimmed === '' || trimmed === setup.org.name) {
      onNext();
      return;
    }
    setBusy(true);
    try {
      onSetup(await apiPost<Setup>('/api/v1/setup/org', { name: trimmed }));
      onNext();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  const src = setup.org.has_logo ? ORG_LOGO_SRC + (logoNonce > 0 ? '?v=' + logoNonce : '') : undefined;

  return (
    <>
      <StepHead title="Your org">The name and mark your team sees in Kivali.</StepHead>
      <TextField label="Org name" hint="Optional. You can change it later in Org." value={name} onChange={(e) => setName(e.target.value)} />
      <LogoField
        name={name.trim() || 'Your org'}
        src={src}
        accept="image/png"
        hint="Optional. PNG, square, at least 64 px a side."
        onFiles={(f) => void uploadLogo(f)}
      />
      {error != null && <ErrorBanner error={error} />}
      <Actions>
        <Button onClick={onBack}>Back</Button>
        <span className="app-setup-spacer" />
        <Button variant="primary" loading={busy} disabled={busy} onClick={() => void next()}>
          Continue
        </Button>
      </Actions>
    </>
  );
}
