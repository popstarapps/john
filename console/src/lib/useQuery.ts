import { useCallback, useEffect, useRef, useState } from "react";
import { subscribe } from "../api/client";

export interface QueryState<T> {
  data: T | undefined;
  loading: boolean;
  error: Error | null;
  /** True only for the first load — later refreshes keep the old data on screen. */
  initial: boolean;
  refresh: () => void;
}

/**
 * Runs an async reader from `api/client` and re-runs it whenever the client's
 * store changes, so a mutation anywhere updates every view that reads it.
 *
 * `deps` behaves like useEffect's: list the values the reader closes over.
 */
export function useQuery<T>(read: () => Promise<T>, deps: unknown[] = []): QueryState<T> {
  const [data, setData] = useState<T | undefined>(undefined);
  const [error, setError] = useState<Error | null>(null);
  const [loading, setLoading] = useState(true);
  const [initial, setInitial] = useState(true);
  const [nonce, setNonce] = useState(0);

  // The reader is redefined on every render by every caller; deps decide when
  // it actually re-runs, exactly as with useEffect.
  const readRef = useRef(read);
  readRef.current = read;

  useEffect(() => {
    let live = true;
    setLoading(true);
    readRef
      .current()
      .then((value) => {
        if (!live) return;
        setData(value);
        setError(null);
      })
      .catch((e: unknown) => {
        if (live) setError(e instanceof Error ? e : new Error(String(e)));
      })
      .finally(() => {
        if (!live) return;
        setLoading(false);
        setInitial(false);
      });
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce]);

  // Any store mutation invalidates every live query.
  useEffect(() => subscribe(() => setNonce((n) => n + 1)), []);

  const refresh = useCallback(() => setNonce((n) => n + 1), []);
  return { data, loading, error, initial, refresh };
}

/** A mutation with its own pending flag, for buttons that must disable.
 *
 * The returned function is stable, but it always runs the latest `fn`: a
 * handler that reads component state must see the state of the render that
 * was clicked, not of the first one. */
export function useAction<A extends unknown[]>(
  fn: (...args: A) => Promise<unknown>,
): [(...args: A) => Promise<void>, boolean] {
  const [busy, setBusy] = useState(false);
  const fnRef = useRef(fn);
  fnRef.current = fn;
  const run = useCallback(async (...args: A) => {
    setBusy(true);
    try {
      await fnRef.current(...args);
    } finally {
      setBusy(false);
    }
  }, []);
  return [run, busy];
}
