"use client";

// Sidebar navigation with active-route highlighting, plus the session
// footer (identity from /api/auth/me + sign out).

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useEffect, useState, type ReactNode } from "react";

import { fetchMe, logout } from "../src/lib/api/client";
import type { SessionUser } from "../src/lib/api/schema";
import { useI18n } from "../src/lib/i18n";

function Icon({ d }: { d: string }) {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d={d} />
    </svg>
  );
}

const ICONS = {
  dashboard: "M2 2h5v5H2zM9 2h5v5H9zM2 9h5v5H2zM9 9h5v5H9z",
  strategies: "M2 12l3.5-4 3 2.5L14 4M14 4h-4M14 4v4",
  reasoning: "M3 3h10v8H8l-3 3v-3H3z",
  settings: "M2 4h12M10 2v4M2 8h12M5 6v4M2 12h12M9 10v4",
  admin: "M8 2l5 1.5V8c0 3-2 4.8-5 6-3-1.2-5-3-5-6V3.5z",
  alerts: "M8 2a4 4 0 0 0-4 4v3l-1.5 2.5h11L12 9V6a4 4 0 0 0-4-4zM6.5 13.5a1.5 1.5 0 0 0 3 0",
} as const;

// Session identity for nav decisions and the footer; a failed fetch leaves
// it null (identity is cosmetic — data fetches own the 401 redirect).
function useSessionUser(): SessionUser | null {
  const [user, setUser] = useState<SessionUser | null>(null);
  useEffect(() => {
    let cancelled = false;
    fetchMe()
      .then((u) => {
        if (!cancelled) setUser(u);
      })
      .catch(() => {
        // identity is cosmetic here; data fetches own the 401 redirect
      });
    return () => {
      cancelled = true;
    };
  }, []);
  return user;
}

function NavItem({
  href,
  icon,
  children,
  exact,
}: {
  href: string;
  icon: keyof typeof ICONS;
  children: ReactNode;
  exact?: boolean;
}) {
  const pathname = usePathname();
  const active = exact ? pathname === href : pathname === href || pathname.startsWith(`${href}/`);
  return (
    <Link href={href} className={`nav-item${active ? " active" : ""}`}>
      <Icon d={ICONS[icon]} />
      {children}
    </Link>
  );
}

export function SidebarNav() {
  // Settings/Admin are platform_admin-only surfaces (tenant owners 403 in
  // v1), so the links are hidden for everyone else.
  const user = useSessionUser();
  const { t } = useI18n();
  return (
    <>
      <div className="nav-group">
        <div className="nav-label">{t("nav.group.operations")}</div>
        <NavItem href="/dashboard" icon="dashboard" exact>
          {t("nav.dashboard")}
        </NavItem>
        <NavItem href="/strategies" icon="strategies">
          {t("nav.strategies")}
        </NavItem>
      </div>
      <div className="nav-group">
        <div className="nav-label">{t("nav.group.audit")}</div>
        <NavItem href="/reasoning" icon="reasoning">
          {t("nav.reasoning")}
        </NavItem>
      </div>
      {user?.role === "platform_admin" && (
        <div className="nav-group">
          <div className="nav-label">{t("nav.group.platform")}</div>
          <NavItem href="/settings" icon="settings">
            {t("nav.settings")}
          </NavItem>
          <NavItem href="/admin" icon="admin">
            {t("nav.admin")}
          </NavItem>
          <NavItem href="/alerts" icon="alerts">
            {t("nav.alerts")}
          </NavItem>
        </div>
      )}
    </>
  );
}

// Bottom-of-sidebar session identity: email + role from /api/auth/me and a
// sign-out button. A failed fetch renders nothing identity-wise (the 401
// path already hard-redirects via usePoll on the page's own data fetches).
export function SessionFooter() {
  const user = useSessionUser();
  const { t } = useI18n();

  async function signOut() {
    try {
      await logout();
    } catch {
      // the cookie may already be gone; still land on /login
    }
    window.location.href = "/login";
  }

  return (
    <div className="session-foot">
      {user && (
        <div className="session-id">
          <span className="session-email" title={user.email} aria-label={user.email}>
            {user.email}
          </span>
          <span className="session-role">{user.role}</span>
        </div>
      )}
      <button type="button" className="btn btn-ghost session-signout" onClick={signOut}>
        {t("nav.signout")}
      </button>
    </div>
  );
}
