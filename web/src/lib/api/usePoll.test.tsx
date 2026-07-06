// @vitest-environment jsdom

// usePoll behavior: initial fetch, interval revalidation, error handling
// (last-known data kept, 401 → /login), refresh(), the hidden-tab pause
// (polled runs skip while document.hidden; visibilitychange back to visible
// fires an immediate catch-up run), and failure backoff (2^n ticks, cap 8×).

import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError } from "./client";
import { usePoll } from "./usePoll";

const INTERVAL = 1_000;

// jsdom's Location is unforgeable; patch document.hidden/visibilityState via
// configurable accessors instead and fire the event the hook listens for.
function setVisibility(hidden: boolean) {
  Object.defineProperty(document, "hidden", { configurable: true, get: () => hidden });
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    get: () => (hidden ? "hidden" : "visible"),
  });
  document.dispatchEvent(new Event("visibilitychange"));
}

// Flushes pending microtasks (load() promise settlement) inside act.
const flush = () => act(async () => {});

describe("usePoll", () => {
  beforeEach(() => {
    // Date must be faked too: the backoff gap check compares Date.now()
    // against the last attempt.
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] });
  });

  afterEach(async () => {
    cleanup();
    await act(async () => {
      setVisibility(false);
    });
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("resolves the initial fetch into data", async () => {
    const load = vi.fn().mockResolvedValue({ total: 3 });
    const { result } = renderHook(() => usePoll(load, INTERVAL));
    expect(load).toHaveBeenCalledTimes(1);
    await flush();
    expect(result.current.data).toEqual({ total: 3 });
    expect(result.current.error).toBeNull();
    expect(result.current.errorStatus).toBeNull();
  });

  it("refetches on the interval", async () => {
    let n = 0;
    const load = vi.fn(() => Promise.resolve(++n));
    const { result } = renderHook(() => usePoll(load, INTERVAL));
    await flush();
    expect(result.current.data).toBe(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(INTERVAL);
    });
    expect(load).toHaveBeenCalledTimes(2);
    expect(result.current.data).toBe(2);
  });

  it("keeps last-known data and sets error+errorStatus on a failed refetch", async () => {
    const load = vi
      .fn()
      .mockResolvedValueOnce("ok")
      .mockRejectedValueOnce(new ApiError(429, { code: "RATE_LIMITED", message: "slow down" }));
    const { result } = renderHook(() => usePoll(load, INTERVAL));
    await flush();
    expect(result.current.data).toBe("ok");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(INTERVAL);
    });
    expect(result.current.data).toBe("ok");
    expect(result.current.error).toBe("RATE_LIMITED: slow down");
    expect(result.current.errorStatus).toBe(429);
  });

  it("redirects to /login on a 401 without surfacing an error", async () => {
    const original = window.location;
    const assign = vi.fn();
    Object.defineProperty(window, "location", {
      configurable: true,
      writable: true,
      value: { ...original, assign },
    });
    try {
      const load = vi.fn().mockRejectedValue(new ApiError(401, null));
      const { result } = renderHook(() => usePoll(load, INTERVAL));
      await flush();
      expect(assign).toHaveBeenCalledWith("/login");
      expect(result.current.error).toBeNull();
      expect(result.current.errorStatus).toBeNull();
    } finally {
      Object.defineProperty(window, "location", {
        configurable: true,
        writable: true,
        value: original,
      });
    }
  });

  it("refresh() triggers an immediate refetch", async () => {
    let n = 0;
    const load = vi.fn(() => Promise.resolve(++n));
    const { result } = renderHook(() => usePoll(load, INTERVAL));
    await flush();
    expect(result.current.data).toBe(1);
    await act(async () => {
      result.current.refresh();
    });
    await flush();
    expect(load).toHaveBeenCalledTimes(2);
    expect(result.current.data).toBe(2);
  });

  it("skips polled fetches while hidden and refetches immediately on return to visible", async () => {
    const load = vi.fn().mockResolvedValue("ok");
    renderHook(() => usePoll(load, INTERVAL));
    await flush();
    expect(load).toHaveBeenCalledTimes(1);

    await act(async () => {
      setVisibility(true);
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3 * INTERVAL);
    });
    expect(load).toHaveBeenCalledTimes(1);

    await act(async () => {
      setVisibility(false);
    });
    expect(load).toHaveBeenCalledTimes(2);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(INTERVAL);
    });
    expect(load).toHaveBeenCalledTimes(3);
  });

  it("backs off exponentially on consecutive failures, capped at 8x the interval", async () => {
    const load = vi.fn().mockRejectedValue(new ApiError(500, null));
    renderHook(() => usePoll(load, INTERVAL));
    await flush();
    expect(load).toHaveBeenCalledTimes(1); // t=0: failure #1

    await act(async () => {
      await vi.advanceTimersByTimeAsync(INTERVAL); // t=1×: skipped (gap is 2×)
    });
    expect(load).toHaveBeenCalledTimes(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(INTERVAL); // t=2×: failure #2
    });
    expect(load).toHaveBeenCalledTimes(2);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3 * INTERVAL); // t=5×: skipped (gap is 4×)
    });
    expect(load).toHaveBeenCalledTimes(2);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(INTERVAL); // t=6×: failure #3
    });
    expect(load).toHaveBeenCalledTimes(3);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(7 * INTERVAL); // t=13×: skipped (gap capped at 8×)
    });
    expect(load).toHaveBeenCalledTimes(3);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(INTERVAL); // t=14×: failure #4
    });
    expect(load).toHaveBeenCalledTimes(4);
  });

  it("restores the base cadence after a success", async () => {
    const load = vi
      .fn()
      .mockRejectedValueOnce(new ApiError(429, { code: "RATE_LIMITED", message: "slow down" }))
      .mockResolvedValue("ok");
    const { result } = renderHook(() => usePoll(load, INTERVAL));
    await flush();
    expect(load).toHaveBeenCalledTimes(1); // t=0: failure

    await act(async () => {
      await vi.advanceTimersByTimeAsync(INTERVAL); // t=1×: skipped (gap is 2×)
    });
    expect(load).toHaveBeenCalledTimes(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(INTERVAL); // t=2×: success resets backoff
    });
    expect(load).toHaveBeenCalledTimes(2);
    expect(result.current.data).toBe("ok");
    expect(result.current.error).toBeNull();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(INTERVAL); // t=3×: base cadence again
    });
    expect(load).toHaveBeenCalledTimes(3);
  });

  it("refresh() after failures fetches immediately", async () => {
    const load = vi.fn().mockRejectedValueOnce(new ApiError(500, null)).mockResolvedValue("ok");
    const { result } = renderHook(() => usePoll(load, INTERVAL));
    await flush();
    expect(load).toHaveBeenCalledTimes(1); // t=0: failure

    await act(async () => {
      await vi.advanceTimersByTimeAsync(INTERVAL); // t=1×: skipped (gap is 2×)
    });
    expect(load).toHaveBeenCalledTimes(1);

    await act(async () => {
      result.current.refresh();
    });
    await flush();
    expect(load).toHaveBeenCalledTimes(2);
    expect(result.current.data).toBe("ok");
    expect(result.current.error).toBeNull();
  });
});
