// Catch-all session proxy: the browser fetches same-origin /api/cp/<path>
// and this handler forwards to `${CP}/api/v1/<path>?<query>` with the
// amx_session cookie attached as the bearer — replacing both the build-time
// /api/v1 rewrites (read token) and the per-route OPERATOR_TOKEN proxies.
// The request body and the upstream status/body are forwarded verbatim; no
// cookie → 401 {"code":"UNAUTHENTICATED","message":"no session"}.

import { crossSiteHeadersFrom, crossSiteRejection } from "../../../../src/lib/api/csrf";
import { forwardWithSession, jsonError } from "../../../../src/lib/api/session";

function cpPath(segments: string[]): string {
  return `/${segments.map(encodeURIComponent).join("/")}`;
}

export async function GET(
  request: Request,
  context: { params: Promise<{ path: string[] }> },
): Promise<Response> {
  const { path } = await context.params;
  return forwardWithSession(request, cpPath(path), "GET");
}

export async function POST(
  request: Request,
  context: { params: Promise<{ path: string[] }> },
): Promise<Response> {
  if (crossSiteRejection("POST", crossSiteHeadersFrom(request.headers))) {
    return jsonError(403, "CSRF_REJECTED", "cross-site request rejected");
  }
  const { path } = await context.params;
  return forwardWithSession(request, cpPath(path), "POST");
}
