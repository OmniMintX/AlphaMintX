// Current session identity: GET /api/v1/auth/me with the cookie bearer.
// No cookie → 401 UNAUTHENTICATED; upstream status/body pass through
// verbatim (a revoked session 401s here and the client redirects to /login).

import { forwardWithSession } from "../../../../src/lib/api/session";

export async function GET(request: Request): Promise<Response> {
  return forwardWithSession(request, "/auth/me", "GET");
}
