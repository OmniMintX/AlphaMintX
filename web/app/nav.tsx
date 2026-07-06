"use client";

// Sidebar navigation with active-route highlighting.

import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";

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
} as const;

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
  return (
    <>
      <div className="nav-group">
        <div className="nav-label">Operations</div>
        <NavItem href="/" icon="dashboard" exact>
          Dashboard
        </NavItem>
        <NavItem href="/strategies" icon="strategies">
          Strategies
        </NavItem>
      </div>
      <div className="nav-group">
        <div className="nav-label">Audit</div>
        <NavItem href="/reasoning" icon="reasoning">
          Reasoning viewer
        </NavItem>
      </div>
    </>
  );
}
