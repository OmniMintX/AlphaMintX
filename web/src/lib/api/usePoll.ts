"use client";

// Simple interval revalidation: the spec defers SSE/websocket and has the web
// dashboard poll the read endpoints (docs/specs/persistence-and-api.md).

import { useCallback, useEffect, useRef, useState } from "react";

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
  // Failure backoff (rate-budget friendly, LC-24): each consecutive failure
  // doubles the gap to the next polled attempt, capped at 8× the interval;
  // one success restores the base cadence.
  const consecutiveFailures = useRef(0);
  const lastAttemptAt = useRef(0);

  useEffect(() => {
    let cancelled = false;
    const run = () => {
      lastAttemptAt.current = Date.now();
      load()
        .then((result) => {
          if (cancelled) return;
          consecutiveFailures.current = 0;
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
          consecutiveFailures.current += 1;
          setError(err instanceof Error ? err.message : String(err));
          setErrorStatus(err instanceof ApiError ? err.status : null);
        });
    };
    // Hidden tabs skip polled runs (rate-budget friendly, LC-24); returning
    // to visible fires an immediate catch-up run, then the interval resumes.
    const tick = () => {
      if (typeof document !== "undefined" && document.hidden) return;
      // Only gate while failing: the healthy path must never skip a tick
      // (timer jitter would otherwise halve the effective cadence). The
      // half-interval slack keeps backoff boundaries jitter-tolerant.
      if (consecutiveFailures.current > 0) {
        const requiredGap = intervalMs * Math.min(2 ** consecutiveFailures.current, 8);
        if (Date.now() - lastAttemptAt.current < requiredGap - intervalMs / 2) return;
      }
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

  // Manual refresh always fetches immediately: the nonce bump re-runs the
  // effect, which calls run() directly (no gap check on that path).
  const refresh = useCallback(() => {
    consecutiveFailures.current = 0;
    setNonce((n) => n + 1);
  }, []);
  return { data, error, errorStatus, refresh };
}
