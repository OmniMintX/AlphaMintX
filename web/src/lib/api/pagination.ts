// Pagination model for the {items, total, page, limit} API shape
// (docs/specs/persistence-and-api.md: page 1-based, limit default 20, max 100).

export const DEFAULT_LIMIT = 20;
export const MAX_LIMIT = 100;

export function clampLimit(limit: number): number {
  if (!Number.isInteger(limit) || limit < 1) return DEFAULT_LIMIT;
  return Math.min(limit, MAX_LIMIT);
}

export function totalPages(total: number, limit: number): number {
  if (total <= 0) return 1;
  return Math.ceil(total / clampLimit(limit));
}

export function hasPrevPage(page: number): boolean {
  return page > 1;
}

export function hasNextPage(page: number, total: number, limit: number): boolean {
  return page < totalPages(total, limit);
}
