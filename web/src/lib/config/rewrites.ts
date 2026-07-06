// Superseded by the /api/cp/[...path] session proxy: the build-time /api/v1
// rewrite existed for the READ-token era, where the browser fetched the
// control-plane directly through the Next server. Every read now rides the
// amx_session cookie through the runtime catch-all route, so exposing a
// cookieless /api/v1 passthrough would only bypass the session layer — this
// always returns no rules and next.config.ts no longer calls it. Delete once
// stale callers are ruled out (this migration could not remove files).
export function controlPlaneRewrites(
  _base: string | undefined,
): { source: string; destination: string }[] {
  return [];
}
