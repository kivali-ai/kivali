import { useEffect } from 'react';

const PRODUCT = 'Kivali';

/** "Kivali" for Home (no page), "Kivali · <page>" everywhere else. */
export function documentTitle(page?: string): string {
  return page ? PRODUCT + ' · ' + page : PRODUCT;
}

/** Sets the browser tab title for as long as the caller is mounted. */
export function useDocumentTitle(page?: string): void {
  useEffect(() => {
    document.title = documentTitle(page);
  }, [page]);
}
