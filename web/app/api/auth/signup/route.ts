// Tenant signup: plain forward of {tenant_name, email, password} — no cookie
// is set here; the client follows up with /api/auth/login. Upstream errors
// (409 EMAIL_TAKEN included) pass through verbatim.

import { forwardAuthPost } from "../../../../src/lib/api/session";

export async function POST(request: Request): Promise<Response> {
  return forwardAuthPost(request, "/auth/signup");
}
