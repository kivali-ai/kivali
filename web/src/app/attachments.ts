import { useCallback } from 'react';
import { useLocation, useNavigate } from 'react-router';
import type { AttachmentRef } from '../api/types.gen';

/** A file the app shows as a formatted document rather than downloading: markdown, by its name. */
export function isMarkdownName(name: string): boolean {
  return /\.(md|markdown|mdown|mkd)$/i.test(name.trim());
}

const ATTACHMENT_URL = /^\/attachments\/([^/?#]+)$/;

/** The app's page showing `a` as a document, or null when its URL is not a stored attachment's. */
export function documentPath(a: AttachmentRef): string | null {
  const m = ATTACHMENT_URL.exec(a.url);
  if (!m) return null;
  return '/documents/' + m[1] + '?name=' + encodeURIComponent(a.name);
}

/** What the document page is opened with: the page its Back returns to. */
export interface DocumentState {
  from: string;
}

/** The page a document page returns to, from its navigation state; Home when it was opened some other way. */
export function documentBack(state: unknown): string {
  const from = state !== null && typeof state === 'object' ? (state as Partial<DocumentState>).from : undefined;
  return typeof from === 'string' && from.startsWith('/') && !from.startsWith('//') ? from : '/';
}

/** Whether clicking `a` opens it as a document. */
export function opensAsDocument(a: AttachmentRef): boolean {
  return isMarkdownName(a.name) && documentPath(a) !== null;
}

/** An attachment button's icon and hover title: open for a document, download for anything else. */
export function attachmentAction(a: AttachmentRef): { icon: 'file-text' | 'download'; title: string } {
  return opensAsDocument(a) ? { icon: 'file-text', title: 'Open ' + a.name } : { icon: 'download', title: 'Download ' + a.name };
}

/**
 * What clicking an attachment does. Markdown opens formatted on a page of the app, with Back to where it was
 * opened. Anything else downloads: the server answers the URL as a download, so the page stays, and whatever saves
 * downloads (the browser, or Kivali Desktop's save sheet) takes the file.
 */
export function useOpenAttachment(): (a: AttachmentRef) => void {
  const navigate = useNavigate();
  const { pathname, search } = useLocation();
  return useCallback(
    (a: AttachmentRef) => {
      const path = opensAsDocument(a) ? documentPath(a) : null;
      if (path) {
        const state: DocumentState = { from: pathname + search };
        void navigate(path, { state });
      } else {
        window.location.assign(a.url);
      }
    },
    [navigate, pathname, search],
  );
}
