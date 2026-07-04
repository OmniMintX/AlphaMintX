// Pagination model: page is 1-based, limit default 20, max 100.

import { describe, expect, it } from "vitest";

import {
  DEFAULT_LIMIT,
  MAX_LIMIT,
  clampLimit,
  hasNextPage,
  hasPrevPage,
  totalPages,
} from "./pagination";

describe("clampLimit", () => {
  it("keeps valid limits and caps at 100", () => {
    expect(clampLimit(20)).toBe(20);
    expect(clampLimit(100)).toBe(MAX_LIMIT);
    expect(clampLimit(250)).toBe(MAX_LIMIT);
  });

  it("falls back to the default for invalid limits", () => {
    expect(clampLimit(0)).toBe(DEFAULT_LIMIT);
    expect(clampLimit(-5)).toBe(DEFAULT_LIMIT);
    expect(clampLimit(2.5)).toBe(DEFAULT_LIMIT);
  });
});

describe("totalPages", () => {
  it("rounds up partial pages", () => {
    expect(totalPages(0, 20)).toBe(1);
    expect(totalPages(20, 20)).toBe(1);
    expect(totalPages(21, 20)).toBe(2);
    expect(totalPages(41, 20)).toBe(3);
  });
});

describe("hasPrevPage / hasNextPage", () => {
  it("bounds navigation at both ends", () => {
    expect(hasPrevPage(1)).toBe(false);
    expect(hasPrevPage(2)).toBe(true);
    expect(hasNextPage(1, 41, 20)).toBe(true);
    expect(hasNextPage(3, 41, 20)).toBe(false);
    expect(hasNextPage(1, 0, 20)).toBe(false);
  });
});
