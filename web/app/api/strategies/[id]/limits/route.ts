// Same-origin limits proxy: forwards the {"changes": {...}} body as received
// with the server-only token attached and returns the upstream status and
// body verbatim. The control plane resolves the token's role — an
// operator-class token gets the pinned 403 passthrough (operator-surface.md
// OS-3/OS-30 precedent), an admin/owner user token succeeds.

import { proxyOperatorPost } from "../../../../../src/lib/api/opsProxy";

export async function POST(
  request: Request,
  context: { params: Promise<{ id: string }> },
): Promise<Response> {
  const { id } = await context.params;
  return proxyOperatorPost(request, `/api/v1/strategies/${encodeURIComponent(id)}/limits`);
}
