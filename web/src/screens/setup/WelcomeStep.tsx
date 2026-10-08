import { useState } from 'react';
import { ApiError, apiPostForm } from '../../api/client';
import { Banner, Button, FileDrop, Text } from '../../ds';
import { Actions, ErrorBanner, StepHead } from './parts';

const WHAT_HAPPENS = [
  { n: '1', title: 'Your org', detail: 'Its name and logo' },
  { n: '2', title: 'Project files', detail: 'What your Chief of Staff will read to learn the business' },
  { n: '3', title: 'Chief of Staff', detail: 'Your first hire; it builds the rest of the team with you' },
];

export interface WelcomeStepProps {
  restoreAvailable: boolean;
  onStart(): void;
}

/** Step 1: what is being set up, and on a fresh install a way in from a backup. */
export function WelcomeStep({ restoreAvailable, onStart }: WelcomeStepProps) {
  const [restoring, setRestoring] = useState(false);
  const [restored, setRestored] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  async function restore(files: File[]) {
    const archive = files[0];
    if (!archive) return;
    // Backups are .zip files; anything else would only come back as the server's parse error.
    if (!archive.name.toLowerCase().endsWith('.zip')) {
      setError(new ApiError(400, 'That is not a Kivali backup.', 'Choose the .zip file you downloaded from Backup in Org.'));
      return;
    }
    const form = new FormData();
    form.append('archive', archive);
    setError(null);
    setBusy(true);
    try {
      await apiPostForm('/api/v1/org/restore', form);
      setRestored(true);
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <StepHead title="Set up your org">Three short things, then your Chief of Staff takes it from there.</StepHead>
      <div className="app-setup-list">
        {WHAT_HAPPENS.map((row) => (
          <div key={row.n} className="app-setup-numbered">
            <Text variant="label" tone="muted">
              {row.n}
            </Text>
            <span className="app-setup-numbered-text">
              <Text tone="default">{row.title}</Text>
              <Text variant="caption" tone="muted">
                {row.detail}
              </Text>
            </span>
          </div>
        ))}
      </div>
      {restoring && !restored && (
        <div className="app-setup-stack">
          <FileDrop label="Drop a backup" hint="The .zip file you downloaded from Kivali" accept=".zip,application/zip" multiple={false} onFiles={(f) => void restore(f)} />
          {busy && (
            <Text as="p" variant="caption" tone="muted" role="status">
              Restoring your org. Large backups take a few minutes.
            </Text>
          )}
          {error != null && <ErrorBanner error={error} />}
        </div>
      )}
      {restored && (
        <Banner
          tone="success"
          title="Backup restored"
          action={
            <Button size="sm" onClick={() => window.location.reload()}>
              Reload
            </Button>
          }
        >
          Your org is back. Reload to pick up where it left off.
        </Banner>
      )}
      <Actions>
        {restoreAvailable && !restoring && (
          <Button variant="ghost" onClick={() => setRestoring(true)}>
            Restore from a backup
          </Button>
        )}
        <span className="app-setup-spacer" />
        <Button variant="primary" onClick={onStart}>
          Start
        </Button>
      </Actions>
    </>
  );
}
