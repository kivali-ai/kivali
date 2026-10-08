import { useState } from 'react';
import { apiDelete, apiPostForm } from '../../api/client';
import type { Setup } from '../../api/types.gen';
import { Button, Checkbox, FileDrop, Icon, ListRow } from '../../ds';
import { formatBytes } from './format';
import { Actions, ErrorBanner, StepHead } from './parts';

export interface FilesStepProps {
  setup: Setup;
  fromFiles: boolean;
  onFromFiles(v: boolean): void;
  onSetup(next: Setup): void;
  onBack(): void;
  onNext(): void;
}

/**
 * Step 3: what your Chief of Staff will read, and whether it should write the handbook from it. On a personal
 * team the files are optional: its Chief of Staff asks a few questions and drafts About you either way.
 */
export function FilesStep({ setup, fromFiles, onFromFiles, onSetup, onBack, onNext }: FilesStepProps) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const orgName = setup.org.name.trim();
  const personal = setup.kind === 'personal';

  async function upload(files: File[]) {
    if (files.length === 0) return;
    const form = new FormData();
    for (const f of files) form.append('files[]', f);
    setBusy(true);
    setError(null);
    try {
      onSetup(await apiPostForm<Setup>('/api/v1/setup/files', form));
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  async function remove(sha: string) {
    setError(null);
    try {
      onSetup(await apiDelete<Setup>('/api/v1/setup/files/' + encodeURIComponent(sha)));
    } catch (err) {
      setError(err);
    }
  }

  return (
    <>
      {personal ? (
        <StepHead title="Anything your Chief of Staff should read">
          Add files if you’d like: notes, a calendar, who’s who. Your Chief of Staff will ask you a few questions either way.
        </StepHead>
      ) : (
        <StepHead title="What your Chief of Staff will read">
          {'Anything that explains the business: a plan, a deck, pricing, contracts. It reads these to learn how ' +
            (orgName || 'your org') +
            ' works.'}
        </StepHead>
      )}
      <FileDrop label="Drop project files" hint="PDF, Markdown, CSV, slides or images, up to 200 MB at a time" onFiles={(f) => void upload(f)} />
      {setup.files.length > 0 && (
        <div className="app-setup-list">
          {setup.files.map((f) => (
            <ListRow
              key={f.sha}
              lead={<Icon name="file-text" size={20} />}
              title={f.name}
              meta={formatBytes(f.size_bytes) + ' · ' + (f.extracted ? 'Read' : 'Still being read')}
              trail={
                <Button variant="ghost" size="sm" aria-label={'Remove ' + f.name} onClick={() => void remove(f.sha)}>
                  Remove
                </Button>
              }
            />
          ))}
        </div>
      )}
      {setup.files.length > 0 && !personal && (
        <div className="app-setup-card">
          <Checkbox
            label="Write the handbook from these files?"
            hint="Your Chief of Staff drafts one after it reads them and sends it to Needs you to approve. If the files are thin, it asks you first."
            checked={fromFiles}
            onCheckedChange={(v) => onFromFiles(v === true)}
          />
        </div>
      )}
      {error != null && <ErrorBanner error={error} />}
      <Actions>
        <Button onClick={onBack}>Back</Button>
        <span className="app-setup-spacer" />
        {!personal && (
          <Button variant="ghost" onClick={onNext}>
            Skip
          </Button>
        )}
        <Button variant="primary" disabled={busy} onClick={onNext}>
          Continue
        </Button>
      </Actions>
    </>
  );
}
