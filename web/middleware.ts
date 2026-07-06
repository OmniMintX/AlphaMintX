// Session gate for page navigation: no amx_session cookie → redirect to
// /login. This checks cookie PRESENCE only — the control-plane is the
// authority; a stale cookie 401s at /api/cp and the client redirects.
// /api/* is excluded via the matcher so data fetches get proper 401 JSON
// from the route handlers instead of an HTML redirect.

import { NextResponse, type NextRequest } from "next/server";

import { SESSION_COOKIE } from "./src/lib/api/session";

// /bootstrap is public alongside /, /login, /signup: the first-run admin
// flow must be reachable before any session exists.
const PUBLIC_PATHS = new Set(["/", "/login", "/signup", "/bootstrap", "/favicon.ico"]);

export function middleware(request: NextRequest): NextResponse {
  const { pathname } = request.nextUrl;
  if (PUBLIC_PATHS.has(pathname) || request.cookies.has(SESSION_COOKIE)) {
    return NextResponse.next();
  }
  const url = request.nextUrl.clone();
  url.pathname = "/login";
  url.search = "";
  return NextResponse.redirect(url);
}

export const config = {
  matcher: ["/((?!api/|_next/|favicon.ico).*)"],
};
