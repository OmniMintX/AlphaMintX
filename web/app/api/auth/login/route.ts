// Session login: forwards credentials to the control-plane, then moves the
// returned bearer token into the HttpOnly amx_session cookie — the response
// body carries {"user": ...} only, never the token. Upstream errors (401
// INVALID_CREDENTIALS included) pass through verbatim.

import { crossSiteHeadersFrom, crossSiteRejection } from "../../../../src/lib/api/csrf";
import {
  NO_STORE,
  cpBaseUrl,
  isSecureRequest,
  jsonError,
  sessionCookie,
  unconfigured,
} from "../../../../src/lib/api/session";

export async function POST(request: Request): Promise<Response> {
  if (crossSiteRejection("POST", crossSiteHeadersFrom(request.headers))) {
    return jsonError(403, "CSRF_REJECTED", "cross-site request rejected");
  }
  const base = cpBaseUrl();
  if (!base) return unconfigured();

  const body = await request.text();
  const upstream = await fetch(`${base}/api/v1/auth/login`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body,
    cache: "no-store",
  });
  const text = await upstream.text();
  if (!upstream.ok) {
    return new Response(text, {
      status: upstream.status,
      headers: {
        "content-type": upstream.headers.get("content-type") ?? "application/json",
        "cache-control": NO_STORE,
      },
    });
  }

  let token: unknown;
  let user: unknown;
  try {
    const json = JSON.parse(text) as { token?: unknown; user?: unknown };
    token = json.token;
    user = json.user;
  } catch {
    // fall through to the malformed-upstream error below
  }
  if (typeof token !== "string" || !token) {
    return jsonError(502, "UPSTREAM_MALFORMED", "login response carried no session token");
  }

  return new Response(JSON.stringify({ user }), {
    status: 200,
    headers: {
      "content-type": "application/json",
      "cache-control": NO_STORE,
      "set-cookie": sessionCookie(token, isSecureRequest(request)),
    },
  });
}
