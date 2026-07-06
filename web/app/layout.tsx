import type { Metadata } from "next";
import type { ReactNode } from "react";

import "./globals.css";
import { AppShell } from "./shell";

export const metadata: Metadata = {
  title: "AlphaMintX",
  description:
    "LLM-driven auto trading: dashboard, reasoning viewer, copilot approvals, kill-switch controls.",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body>
        <AppShell>{children}</AppShell>
      </body>
    </html>
  );
}
