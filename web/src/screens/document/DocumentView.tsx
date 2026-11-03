import { useCallback, useEffect, useMemo, useState } from 'react';
import { useLocation, useParams, useSearchParams } from 'react-router';
import { ApiError, apiGetText } from '../../api/client';
import { documentBack } from '../../app/attachments';
import { useFrameChrome } from '../../app/chrome';
import { PageTitle } from '../../app/PageTitle';
import { Banner, Button, Icon, Prose, Skeleton, Text } from '../../ds';
import { renderMarkdown } from '../../lib/markdown';
import { asSentence } from '../../state/home';
import '../../styles/document.css';

/** The most text the page formats; a longer file is offered as a download only. */
export const DOCUMENT_MAX_CHARS = 1_000_000;

type Loaded = { kind: 'loading' } | { kind: 'text'; text: string } | { kind: 'error'; error: ApiError };

function asError(err: unknown): ApiError {
  if (err instanceof ApiError && err.status === 404) {
    return new ApiError(404, 'This file isn’t here.', 'The message that carried it may have been deleted.');
  }
  return err instanceof ApiError ? err : new ApiError(0, 'Kivali could not read this file.', 'Download it to read it.');
}

/**
 * A markdown attachment, formatted as a document: its name, a Download button, and the text rendered as a message
 * body is (sanitized), at reading width. Back returns to the page it was opened from.
 */
export function DocumentView() {
  const { sha = '' } = useParams();
  const [params] = useSearchParams();
  const name = params.get('name') || 'Document';
  const back = documentBack(useLocation().state);
  useFrameChrome({ title: name, tabBar: false, width: 'reading', back: { to: back, label: 'Back' } });

  const url = '/attachments/' + encodeURIComponent(sha);
  const [loaded, setLoaded] = useState<Loaded>({ kind: 'loading' });
  const load = useCallback(
    (signal?: AbortSignal) => {
      setLoaded({ kind: 'loading' });
      apiGetText(url, signal ? { signal } : {}).then(
        (text) => setLoaded({ kind: 'text', text }),
        (err: unknown) => {
          if (err instanceof DOMException && err.name === 'AbortError') return;
          setLoaded({ kind: 'error', error: asError(err) });
        },
      );
    },
    [url],
  );
  useEffect(() => {
    const ac = new AbortController();
    load(ac.signal);
    return () => ac.abort();
  }, [load]);

  const tooLarge = loaded.kind === 'text' && loaded.text.length > DOCUMENT_MAX_CHARS;
  const html = useMemo(() => (loaded.kind === 'text' && !tooLarge ? renderMarkdown(loaded.text) : ''), [loaded, tooLarge]);

  return (
    <article className="app-document">
      <header className="app-document-head">
        <PageTitle>{name}</PageTitle>
        <Button variant="secondary" size="sm" icon={<Icon name="download" />} onClick={() => window.location.assign(url)}>
          Download
        </Button>
      </header>
      {loaded.kind === 'loading' && (
        <div className="app-document-skeleton" aria-hidden="true">
          <Skeleton width="70%" />
          <Skeleton width="90%" />
          <Skeleton width="55%" />
        </div>
      )}
      {loaded.kind === 'error' && (
        <Banner
          tone="danger"
          title={asSentence(loaded.error.message)}
          action={
            <Button size="sm" variant="secondary" onClick={() => load()}>
              Try again
            </Button>
          }
        >
          {loaded.error.who}
        </Banner>
      )}
      {tooLarge && (
        <Text as="p" tone="muted">
          This file is too large to show here. Download it to read it.
        </Text>
      )}
      {loaded.kind === 'text' && !tooLarge && !html && (
        <Text as="p" tone="muted">
          This file is empty.
        </Text>
      )}
      {html && <Prose html={html} className="app-document-body" />}
    </article>
  );
}
