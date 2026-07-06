import type { Metadata } from "next";
import type { ReactNode } from "react";

import "./globals.css";
import { SidebarNav } from "./nav";

export const metadata: Metadata = {
  title: "AlphaMintX",
  description:
    "LLM-driven auto trading: dashboard, reasoning viewer, copilot approvals, kill-switch controls.",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body>
        <div className="shell">
          <aside className="sidebar">
            <div className="sidebar-brand">
              <span className="logo">A</span>
              AlphaMintX
              <span className="env-tag">TESTNET</span>
            </div>
            <SidebarNav />
            <div className="sidebar-foot">
              plane boundary enforced
              <br />
              LLMs never touch orders
            </div>
          </aside>
          <main className="main">{children}</main>
        </div>
      </body>
    </html>
  );
}
