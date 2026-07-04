// Same-origin proxy rules (persistence-and-api.md §Auth): the control-plane
// serves no CORS headers by design, so the browser reaches it through the
// Next server. Reuses CONTROLPLANE_API_BASE_URL — the same server-side var
// the approvals route handler already requires. Rewrites are baked into the
// build (routes-manifest.json), so the var must be set at `next build` time.
export function controlPlaneRewrites(
  base: string | undefined,
): { source: string; destination: string }[] {
  const origin = base?.trim().replace(/\/+$/, "");
  if (!origin) return [];
  return [{ source: "/api/v1/:path*", destination: `${origin}/api/v1/:path*` }];
}
