// Server-side session plumbing for the SaaS shell: the control-plane bearer
// token lives in the HttpOnly "amx_session" cookie set by /api/auth/login —
// the browser NEVER sees it. Route handlers read the cookie here, attach the
// bearer server-side, and return the upstream status and body verbatim (the
// old operator-proxy precedent: errors are never remapped or retried).

export const SESSION_COOKIE = "amx_session";

// Cookie lifetime: 7 days. The control-plane session is the authority — a
// revoked-but-unexpired cookie simply 401s on the next /api/cp fetch.
export const SESSION_MAX_AGE_SECONDS = 7 * 24 * 60 * 60;

// The control-plane origin as seen from the Next.js SERVER (server-side env
// only — never NEXT_PUBLIC_, which Next.js inlines into the client bundle).
export function cpBaseUrl(): string | undefined {
  const origin = process.env.CONTROLPLANE_API_BASE_URL?.trim().replace(/\/+$/, "");
  return origin || undefined;
}

// The session token from the request's Cookie header, or null.
export function sessionTokenFrom(request: Request): string | null {
  const header = request.headers.get("cookie");
  if (!header) return null;
  for (const part of header.split(";")) {
    const eq = part.indexOf("=");
    if (eq === -1) continue;
    if (part.slice(0, eq).trim() === SESSION_COOKIE) {
      return decodeURIComponent(part.slice(eq + 1).trim());
    }
  }
  return null;
}

// Secure iff the fronting proxy says the request arrived over TLS.
export function isSecureRequest(request: Request): boolean {
  return request.headers.get("x-forwarded-proto") === "https";
}

export function sessionCookie(token: string, secure: boolean): string {
  const parts = [
    `${SESSION_COOKIE}=${encodeURIComponent(token)}`,
    "Path=/",
    "HttpOnly",
    "SameSite=Lax",
    `Max-Age=${SESSION_MAX_AGE_SECONDS}`,
  ];
  if (secure) parts.push("Secure");
  return parts.join("; ");
}

export function clearedSessionCookie(secure: boolean): string {
  const parts = [`${SESSION_COOKIE}=`, "Path=/", "HttpOnly", "SameSite=Lax", "Max-Age=0"];
  if (secure) parts.push("Secure");
  return parts.join("; ");
}

// Every session-proxy response carries authenticated (or auth-adjacent) data;
// nothing here is safe for a shared cache (RFC 9111 §3.5). Next.js does NOT
// add Cache-Control to route-handler responses — it must be explicit.
export const NO_STORE = "no-store" as const;

export function jsonError(status: number, code: string, message: string): Response {
  return new Response(JSON.stringify({ code, message }), {
    status,
    headers: { "content-type": "application/json", "cache-control": NO_STORE },
  });
}

export function unconfigured(): Response {
  return jsonError(
    503,
    "CP_PROXY_UNCONFIGURED",
    "the session proxy requires CONTROLPLANE_API_BASE_URL (server-side env)",
  );
}

function passthrough(upstream: Response, text: string): Response {
  return new Response(text, {
    status: upstream.status,
    headers: {
      "content-type": upstream.headers.get("content-type") ?? "application/json",
      "cache-control": NO_STORE,
    },
  });
}

// Plain forward of an anonymous auth POST (signup/bootstrap): body as
// received, upstream status/body verbatim, no cookie involved.
export async function forwardAuthPost(request: Request, cpPath: string): Promise<Response> {
  const base = cpBaseUrl();
  if (!base) return unconfigured();
  const body = await request.text();
  const upstream = await fetch(`${base}/api/v1${cpPath}`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body,
    cache: "no-store",
  });
  return passthrough(upstream, await upstream.text());
}

// Session-authenticated forward to `${CP}/api/v1<cpPath>?<query>`: the query
// string is threaded verbatim from the incoming request and the amx_session
// cookie becomes the bearer. No cookie → 401 UNAUTHENTICATED (the client
// redirects to /login on that status).
export async function forwardWithSession(
  request: Request,
  cpPath: string,
  method: "GET" | "POST",
): Promise<Response> {
  const token = sessionTokenFrom(request);
  if (!token) return jsonError(401, "UNAUTHENTICATED", "no session");
  const base = cpBaseUrl();
  if (!base) return unconfigured();

  const headers: Record<string, string> = { authorization: `Bearer ${token}` };
  let body: string | undefined;
  if (method === "POST") {
    headers["content-type"] = "application/json";
    body = await request.text();
  }
  const { search } = new URL(request.url);
  const upstream = await fetch(`${base}/api/v1${cpPath}${search}`, {
    method,
    headers,
    body,
    cache: "no-store",
  });
  return passthrough(upstream, await upstream.text());
}
