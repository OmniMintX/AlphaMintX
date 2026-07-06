"use client";

// Layout split for the session shell: public surfaces (landing, login,
// signup, bootstrap) render plain, everything else gets the sidebar app
// shell. Route groups would be the idiomatic split, but this migration
// could not move files — the set here mirrors middleware.ts PUBLIC_PATHS
// (minus /favicon.ico, which never renders a layout).

import { usePathname } from "next/navigation";
import type { ReactNode } from "react";

import { SessionFooter, SidebarNav } from "./nav";

const PUBLIC_PATHS = new Set(["/", "/login", "/signup", "/bootstrap"]);

export function AppShell({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  if (PUBLIC_PATHS.has(pathname)) return <>{children}</>;
  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="sidebar-brand">
          <span className="logo">A</span>
          AlphaMintX
          <span className="env-tag">TESTNET</span>
        </div>
        <SidebarNav />
        <SessionFooter />
        <div className="sidebar-foot">
          plane boundary enforced
          <br />
          LLMs never touch orders
        </div>
      </aside>
      <main className="main">{children}</main>
    </div>
  );
}
