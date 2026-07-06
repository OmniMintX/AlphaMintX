// Cross-site request rejection for state-changing route handlers: browsers
// always attach Origin to cross-site POSTs, so an Origin (or, failing that,
// Referer) whose host differs from the request's effective host is rejected.
// Requests carrying neither header are allowed — non-browser clients (curl,
// smoke tests) send none, and a browser-mounted CSRF cannot omit Origin.

export interface CrossSiteHeaders {
  origin: string | null;
  referer: string | null;
  host: string | null;
  forwardedHost: string | null;
}

const SAFE_METHODS = new Set(["GET", "HEAD", "OPTIONS"]);

// First value of a possibly comma-separated header (proxies may append).
function firstValue(raw: string | null): string | null {
  if (!raw) return null;
  const first = raw.split(",")[0]?.trim();
  return first || null;
}

// host[:port] from a URL string, or null when unparseable (covers the
// literal Origin "null" sent from opaque/sandboxed contexts).
function urlHost(value: string): string | null {
  try {
    return new URL(value).host || null;
  } catch {
    return null;
  }
}

export function crossSiteHeadersFrom(headers: {
  get(name: string): string | null;
}): CrossSiteHeaders {
  return {
    origin: headers.get("origin"),
    referer: headers.get("referer"),
    host: headers.get("host"),
    forwardedHost: headers.get("x-forwarded-host"),
  };
}

// Returns a reason string when the request must be rejected, or null to
// allow. The scheme is deliberately ignored — the fronting proxy terminates
// TLS, so Origin says https while Host is plain. The Origin/Referer host may
// match EITHER x-forwarded-host or Host: reverse proxies differ on whether
// they preserve the public Host or rewrite it and record the original in
// x-forwarded-host, and a cross-site attacker controls neither header (forms
// cannot set headers; fetch with custom headers fails CORS preflight).
export function crossSiteRejection(method: string, headers: CrossSiteHeaders): string | null {
  if (SAFE_METHODS.has(method.toUpperCase())) return null;

  const allowedHosts = [firstValue(headers.forwardedHost), firstValue(headers.host)].filter(
    (h): h is string => h !== null,
  );

  if (headers.origin !== null) {
    const originHost = urlHost(headers.origin);
    if (!originHost) return `unparseable origin "${headers.origin}"`;
    if (!allowedHosts.includes(originHost)) {
      return `origin host "${originHost}" does not match request host(s) [${allowedHosts.join(", ") || "(none)"}]`;
    }
    return null;
  }

  if (headers.referer !== null) {
    const refererHost = urlHost(headers.referer);
    if (!refererHost) return `unparseable referer "${headers.referer}"`;
    if (!allowedHosts.includes(refererHost)) {
      return `referer host "${refererHost}" does not match request host(s) [${allowedHosts.join(", ") || "(none)"}]`;
    }
    return null;
  }

  return null;
}
