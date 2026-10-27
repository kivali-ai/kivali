import { useEffect, useState } from 'react';
import { ApiError, apiPost } from '../../api/client';
import type { BackupLink } from '../../api/types.gen';
import { Button, Icon, Text } from '../../ds';
import { ErrorBanner, SectionHead } from './parts';
import type { SectionProps } from './sections';

const BACKUP_LINKS_PATH = '/api/v1/org/backup/links';
/** How long the button stays off after a click, so a double click starts one backup. */
export const BACKUP_DEBOUNCE_MS = 2000;

const NOT_STARTED = 'The backup did not start.';
const NOT_STARTED_WHO = 'Try again. If it keeps happening, whoever runs this Kivali server can look into it.';

/**
 * Download only: restoring a backup belongs to a fresh install's first setup screen.
 *
 * The archive goes from the server straight to whatever saves downloads, never through the page: the page asks the
 * server for a one-time link, then sends the browser there, and the attachment that comes back is saved the way any
 * download is (the browser's downloads; in Kivali Desktop, where the person chooses, with its progress shown). The
 * download keeps going if this tab is closed.
 */
export function BackupSection({ showHead }: SectionProps) {
  const [error, setError] = useState<ApiError | null>(null);
  const [started, setStarted] = useState(false);
  const [cooling, setCooling] = useState(false);

  useEffect(() => {
    if (!cooling) return;
    const t = setTimeout(() => setCooling(false), BACKUP_DEBOUNCE_MS);
    return () => clearTimeout(t);
  }, [cooling]);

  const download = () => {
    setError(null);
    setCooling(true);
    void (async () => {
      try {
        const link = await apiPost<BackupLink>(BACKUP_LINKS_PATH, {});
        setStarted(true);
        window.location.assign(link.url);
      } catch (e) {
        setStarted(false);
        setError(e instanceof ApiError ? e : new ApiError(0, NOT_STARTED, NOT_STARTED_WHO));
      }
    })();
  };

  return (
    <>
      <SectionHead title="Backup and restore" show={showHead} />
      <ErrorBanner error={error} onDismiss={() => setError(null)} />
      <Text as="p" variant="caption" tone="muted" className="app-org-plain">
        Everything Kivali keeps for this org, as one zip file. Restore is offered only on a fresh install, from the first setup screen.
      </Text>
      <div className="app-org-actions">
        <Button variant="secondary" icon={<Icon name="download" />} onClick={download} disabled={cooling}>
          Download a backup
        </Button>
      </div>
      {started && (
        <Text as="p" variant="caption" tone="muted" className="app-org-plain" role="status">
          The backup is downloading and shows its progress where your downloads do. A large org takes several minutes. You can close this tab or
          keep working; if the download stops partway, start another.
        </Text>
      )}
    </>
  );
}
