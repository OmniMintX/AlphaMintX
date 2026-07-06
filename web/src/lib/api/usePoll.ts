"use client";

// Simple interval revalidation: the spec defers SSE/websocket and has the web
// dashboard poll the read endpoints (docs/specs/persistence-and-api.md).

import { useCallback, useEffect, useState } from "react";

import { ApiError, POLL_INTERVAL_MS } from "./client";

export interface PollState<T> {
  data: T | null;
  error: string | null;
  // HTTP status of the last failure when it was an ApiError (structured
  // 429 detection, operator-surface.md OS-25); null otherwise.
  errorStatus: number | null;
  refresh: () => void;
}

export function usePoll<T>(load: () => Promise<T>, intervalMs = POLL_INTERVAL_MS): PollState<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [errorStatus, setErrorStatus] = useState<number | null>(null);
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    let cancelled = false;
    const run = () => {
      load()
        .then((result) => {
          if (cancelled) return;
          setData(result);
          setError(null);
          setErrorStatus(null);
        })
        .catch((err: unknown) => {
          if (cancelled) return;
          // A 401 means no/expired session (the middleware only checks cookie
          // presence; the control-plane is the authority) — go sign in.
          if (err instanceof ApiError && err.status === 401 && typeof window !== "undefined") {
            window.location.assign("/login");
            return;
          }
          setError(err instanceof Error ? err.message : String(err));
          setErrorStatus(err instanceof ApiError ? err.status : null);
        });
    };
    run();
    const id = setInterval(run, intervalMs);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [load, intervalMs, nonce]);

  const refresh = useCallback(() => setNonce((n) => n + 1), []);
  return { data, error, errorStatus, refresh };
}
