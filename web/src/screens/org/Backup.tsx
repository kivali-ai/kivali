import { useEffect, useRef, useState } from 'react';
import { ApiError } from '../../api/client';
import { Button, Icon, Text } from '../../ds';
import { ErrorBanner, SectionHead } from './parts';
import type { SectionProps } from './sections';

const BACKUP_PATH = '/api/v1/org/backup';
const FRAME_NAME = 'kivali-backup-download';
/** How long the button stays off after a click, so a double click starts one backup. */
export const BACKUP_DEBOUNCE_MS = 2000;

const NOT_STARTED = 'The backup did not start.';
const NOT_STARTED_WHO = 'Try again. If it keeps happening, whoever runs this Kivali server can look into it.';

/**
 * What the server answered in place of the archive, read from the frame it landed in. A download never
 * loads the frame; only a page does (a JSON refusal, or the browser's own error page).
 */
function frameError(frame: HTMLIFrameElement): ApiError {
  let text = '';
  try {
    text = frame.contentDocument?.body?.textContent ?? '';
  } catch {
    // The browser's error page is not ours to read.
  }
  try {
    const body = JSON.parse(text) as { error?: unknown; who?: unknown };
    if (typeof body.error === 'string' && body.error) {
      return new ApiError(0, body.error, typeof body.who === 'string' && body.who ? body.who : NOT_STARTED_WHO, body);
    }
  } catch {
    // Not a JSON refusal.
  }
  return new ApiError(0, NOT_STARTED, NOT_STARTED_WHO);
}

/**
 * Download only: restoring a backup belongs to a fresh install's first setup screen.
 *
 * The archive goes from the server straight to the browser's own downloads, never through the page: a real
 * form POST into a hidden frame, which the browser turns into a download when the attachment arrives. The
 * download keeps going if this tab is closed.
 */
export function BackupSection({ showHead }: SectionProps) {
  const form = useRef<HTMLFormElement>(null);
  const frame = useRef<HTMLIFrameElement>(null);
  const submitted = useRef(false);
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
    submitted.current = true;
    form.current?.submit();
    setStarted(true);
    setCooling(true);
  };

  // The frame's first about:blank load is not an answer.
  const onFrameLoad = () => {
    if (!submitted.current || !frame.current) return;
    submitted.current = false;
    setStarted(false);
    setError(frameError(frame.current));
  };

  return (
    <>
      <SectionHead title="Backup and restore" show={showHead} />
      <ErrorBanner error={error} onDismiss={() => setError(null)} />
      <Text as="p" variant="caption" tone="muted" className="app-org-plain">
        Everything Kivali keeps for this org, as one zip file. Restore is offered only on a fresh install, from the first setup screen.
      </Text>
      <form ref={form} method="post" action={BACKUP_PATH} target={FRAME_NAME} hidden />
      <iframe ref={frame} name={FRAME_NAME} title="Backup download" hidden onLoad={onFrameLoad} />
      <div className="app-org-actions">
        <Button variant="secondary" icon={<Icon name="download" />} onClick={download} disabled={cooling}>
          Download a backup
        </Button>
      </div>
      {started && (
        <Text as="p" variant="caption" tone="muted" className="app-org-plain" role="status">
          The backup is downloading in your browser’s downloads, where it shows its progress. A large org takes several minutes. You can close this
          tab or keep working; if the download stops partway, the browser marks it failed and you can start another.
        </Text>
      )}
    </>
  );
}
