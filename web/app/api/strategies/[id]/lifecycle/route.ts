// Same-origin lifecycle proxy (operator-surface.md OS-27): forwards the
// LC-4 body as received with the server-only OPERATOR_TOKEN attached and
// returns the upstream status and body verbatim.

import { proxyOperatorPost } from "../../../../../src/lib/api/opsProxy";

export async function POST(
  request: Request,
  context: { params: Promise<{ id: string }> },
): Promise<Response> {
  const { id } = await context.params;
  return proxyOperatorPost(request, `/api/v1/strategies/${encodeURIComponent(id)}/lifecycle`);
}
