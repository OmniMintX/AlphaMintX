// Server-side ops proxy shared by the lifecycle/kill/clear route handlers
// (operator-surface.md OS-27..OS-30) — the approvals-proxy pattern verbatim:
// the OPERATOR_TOKEN lives in a server-only env var (never NEXT_PUBLIC_,
// which Next.js inlines into the client bundle); the browser POSTs to the
// same-origin route and the token is attached here. The body is forwarded as
// received and the upstream status and body are returned verbatim —
// including 400/403/404/409/422/429 (invariant 5: the OS-3 403 under an
// operator-class token is pinned passthrough, never remapped).

import { buildUrl } from "./client";

// The control-plane origin as seen from the Next.js SERVER (a relative ""
// same-origin base only works in the browser).
function serverApiBaseUrl(): string | undefined {
  return (
    process.env.CONTROLPLANE_API_BASE_URL || process.env.NEXT_PUBLIC_API_BASE_URL || undefined
  );
}

// One 503 code for the three ops proxies (OS-27, the
// APPROVAL_PROXY_UNCONFIGURED shape).
export async function proxyOperatorPost(request: Request, upstreamPath: string): Promise<Response> {
  const base = serverApiBaseUrl();
  const token = process.env.OPERATOR_TOKEN;
  if (!base || !token) {
    return new Response(
      JSON.stringify({
        code: "OPS_PROXY_UNCONFIGURED",
        message:
          "ops controls require OPERATOR_TOKEN and CONTROLPLANE_API_BASE_URL (server-side env)",
      }),
      { status: 503, headers: { "content-type": "application/json" } },
    );
  }

  const body = await request.text();
  const upstream = await fetch(buildUrl(base, upstreamPath), {
    method: "POST",
    headers: { "content-type": "application/json", authorization: `Bearer ${token}` },
    body,
    cache: "no-store",
  });
  const text = await upstream.text();
  return new Response(text, {
    status: upstream.status,
    headers: { "content-type": upstream.headers.get("content-type") ?? "application/json" },
  });
}
