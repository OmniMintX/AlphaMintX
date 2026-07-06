// Session logout: best-effort upstream revocation with the cookie bearer,
// then the cookie is cleared unconditionally — a dead control-plane must
// never trap a browser in a session it cannot end.

import { crossSiteHeadersFrom, crossSiteRejection } from "../../../../src/lib/api/csrf";
import {
  clearedSessionCookie,
  cpBaseUrl,
  isSecureRequest,
  jsonError,
  sessionTokenFrom,
} from "../../../../src/lib/api/session";

export async function POST(request: Request): Promise<Response> {
  if (crossSiteRejection("POST", crossSiteHeadersFrom(request.headers))) {
    return jsonError(403, "CSRF_REJECTED", "cross-site request rejected");
  }
  const token = sessionTokenFrom(request);
  const base = cpBaseUrl();
  if (token && base) {
    try {
      await fetch(`${base}/api/v1/auth/logout`, {
        method: "POST",
        headers: { authorization: `Bearer ${token}` },
        cache: "no-store",
      });
    } catch {
      // cookie is cleared regardless
    }
  }
  return new Response(JSON.stringify({}), {
    status: 200,
    headers: {
      "content-type": "application/json",
      "set-cookie": clearedSessionCookie(isSecureRequest(request)),
    },
  });
}
