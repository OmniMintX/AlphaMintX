// Same-origin strategy-tier clear proxy (operator-surface.md OS-29): forwards
// the LC-30 {"reason", "observed_epoch"} body as received with the
// server-only OPERATOR_TOKEN attached and returns the upstream status and
// body verbatim — the 409 CLEAR_CONFLICT included.

import { proxyOperatorPost } from "../../../../../../src/lib/api/opsProxy";

export async function POST(
  request: Request,
  context: { params: Promise<{ id: string }> },
): Promise<Response> {
  const { id } = await context.params;
  return proxyOperatorPost(request, `/api/v1/strategies/${encodeURIComponent(id)}/kill/clear`);
}
