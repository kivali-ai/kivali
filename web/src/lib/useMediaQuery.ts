import { useCallback, useSyncExternalStore } from 'react';

function list(query: string): MediaQueryList | null {
  return typeof window !== 'undefined' && typeof window.matchMedia === 'function' ? window.matchMedia(query) : null;
}

/**
 * Whether a media query matches, following changes. False where matchMedia is missing. Use it only where a
 * width changes what is rendered (a component variant, a count); pure layout belongs in CSS @media.
 */
export function useMediaQuery(query: string): boolean {
  const subscribe = useCallback(
    (notify: () => void) => {
      const mql = list(query);
      if (!mql) return () => {};
      mql.addEventListener('change', notify);
      return () => mql.removeEventListener('change', notify);
    },
    [query],
  );
  const snapshot = () => list(query)?.matches ?? false;
  return useSyncExternalStore(subscribe, snapshot, () => false);
}
