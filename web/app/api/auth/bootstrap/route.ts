// First-run platform-admin bootstrap: plain forward of {email, password} —
// no cookie is set here; the client follows up with /api/auth/login.
// Upstream errors (409 CONFLICT once bootstrapped) pass through verbatim.

import { forwardAuthPost } from "../../../../src/lib/api/session";

export async function POST(request: Request): Promise<Response> {
  return forwardAuthPost(request, "/auth/bootstrap");
}
