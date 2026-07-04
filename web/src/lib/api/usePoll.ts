"use client";

// Simple interval revalidation: the spec defers SSE/websocket and has the web
// dashboard poll the read endpoints (docs/specs/persistence-and-api.md).

import { useCallback, useEffect, useState } from "react";

import { POLL_INTERVAL_MS } from "./client";

export interface PollState<T> {
  data: T | null;
  error: string | null;
  refresh: () => void;
}

export function usePoll<T>(load: () => Promise<T>, intervalMs = POLL_INTERVAL_MS): PollState<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    let cancelled = false;
    const run = () => {
      load()
        .then((result) => {
          if (cancelled) return;
          setData(result);
          setError(null);
        })
        .catch((err: unknown) => {
          if (cancelled) return;
          setError(err instanceof Error ? err.message : String(err));
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
  return { data, error, refresh };
}
