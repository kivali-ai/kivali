import { useCallback, useEffect, useRef, useState } from 'react';
import { ApiError, apiGet } from '../../api/client';

export function asApiError(err: unknown): ApiError {
  return err instanceof ApiError ? err : new ApiError(0, 'Something went wrong in Kivali.', 'Reload the page. If it keeps happening, whoever runs this Kivali server can look into it.');
}

export interface Resource<T> {
  data: T | null;
  error: ApiError | null;
  /** Replace the data with a list a mutation returned. */
  set(next: T): void;
}

/** Loads one Org list on mount. Mutations hand back the whole list, which goes straight in through `set`. */
export function useResource<T>(path: string): Resource<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  useEffect(() => {
    const ctl = new AbortController();
    apiGet<T>(path, { signal: ctl.signal }).then(
      (v) => {
        setData(v);
        setError(null);
      },
      (err: unknown) => {
        if (err instanceof DOMException && err.name === 'AbortError') return;
        setError(asApiError(err));
      },
    );
    return () => ctl.abort();
  }, [path]);
  const set = useCallback((next: T) => {
    setData(next);
    setError(null);
  }, []);
  return { data, error, set };
}

export interface Action {
  /** The last failure, until the next attempt or a dismissal. */
  error: ApiError | null;
  busy: boolean;
  /** Resolves true when `fn` succeeded. */
  run(fn: () => Promise<void>): Promise<boolean>;
  dismiss(): void;
}

/** One in-flight mutation at a time, with its failure kept for a Banner. */
export function useAction(): Action {
  const [error, setError] = useState<ApiError | null>(null);
  const [busy, setBusy] = useState(false);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  const run = useCallback(async (fn: () => Promise<void>): Promise<boolean> => {
    setBusy(true);
    setError(null);
    try {
      await fn();
      return true;
    } catch (err) {
      if (alive.current) setError(asApiError(err));
      return false;
    } finally {
      if (alive.current) setBusy(false);
    }
  }, []);
  const dismiss = useCallback(() => setError(null), []);
  return { error, busy, run, dismiss };
}
