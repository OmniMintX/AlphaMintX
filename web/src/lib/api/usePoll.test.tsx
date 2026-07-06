// @vitest-environment jsdom

// usePoll behavior: initial fetch, interval revalidation, error handling
// (last-known data kept, 401 → /login), refresh(), and the hidden-tab pause
// (polled runs skip while document.hidden; visibilitychange back to visible
// fires an immediate catch-up run).

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
    vi.useFakeTimers();
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
});
