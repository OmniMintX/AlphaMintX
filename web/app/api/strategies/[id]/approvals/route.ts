// Server-side approval proxy (persistence-and-api.md §Auth): READ and
// OPERATOR credentials are separated so a leaked or XSS'd dashboard
// credential can never approve trades. The OPERATOR_TOKEN therefore lives in
// a server-only env var (never NEXT_PUBLIC_, which Next.js inlines into the
// client bundle): the browser POSTs to this same-origin route and the token
// is attached here. The control-plane response — including 409
// already-decided bodies — is passed through verbatim.

import { buildUrl } from "../../../../../src/lib/api/client";

// The control-plane origin as seen from the Next.js SERVER (a relative ""
// same-origin base only works in the browser).
function serverApiBaseUrl(): string | undefined {
  return (
    process.env.CONTROLPLANE_API_BASE_URL || process.env.NEXT_PUBLIC_API_BASE_URL || undefined
  );
}

export async function POST(
  request: Request,
  context: { params: Promise<{ id: string }> },
): Promise<Response> {
  const { id } = await context.params;
  const base = serverApiBaseUrl();
  const token = process.env.OPERATOR_TOKEN;
  if (!base || !token) {
    return new Response(
      JSON.stringify({
        code: "APPROVAL_PROXY_UNCONFIGURED",
        message:
          "approvals require OPERATOR_TOKEN and CONTROLPLANE_API_BASE_URL (server-side env)",
      }),
      { status: 503, headers: { "content-type": "application/json" } },
    );
  }

  const body = await request.text();
  const upstream = await fetch(
    buildUrl(base, `/api/v1/strategies/${encodeURIComponent(id)}/approvals`),
    {
      method: "POST",
      headers: { "content-type": "application/json", authorization: `Bearer ${token}` },
      body,
      cache: "no-store",
    },
  );
  const text = await upstream.text();
  return new Response(text, {
    status: upstream.status,
    headers: { "content-type": upstream.headers.get("content-type") ?? "application/json" },
  });
}
