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
    // Hidden tabs skip polled runs (rate-budget friendly, LC-24); returning
    // to visible fires an immediate catch-up run, then the interval resumes.
    const tick = () => {
      if (typeof document !== "undefined" && document.hidden) return;
      run();
    };
    const onVisibility = () => {
      if (!document.hidden) run();
    };
    run();
    const id = setInterval(tick, intervalMs);
    if (typeof document !== "undefined") {
      document.addEventListener("visibilitychange", onVisibility);
    }
    return () => {
      cancelled = true;
      clearInterval(id);
      if (typeof document !== "undefined") {
        document.removeEventListener("visibilitychange", onVisibility);
      }
    };
  }, [load, intervalMs, nonce]);

  const refresh = useCallback(() => setNonce((n) => n + 1), []);
  return { data, error, errorStatus, refresh };
}
