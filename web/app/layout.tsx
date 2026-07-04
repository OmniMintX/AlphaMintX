import type { Metadata } from "next";
import type { ReactNode } from "react";

export const metadata: Metadata = {
  title: "AlphaMintX",
  description:
    "LLM-driven auto trading: dashboard, reasoning viewer, copilot approvals, kill-switch controls.",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body
        style={{
          margin: 0,
          fontFamily:
            "system-ui, -apple-system, 'Segoe UI', Roboto, 'Helvetica Neue', sans-serif",
          color: "#1a1a1a",
          background: "#fafafa",
        }}
      >
        <header
          style={{
            padding: "0.75rem 1.5rem",
            borderBottom: "1px solid #e0e0e0",
            display: "flex",
            alignItems: "baseline",
            gap: "1.5rem",
          }}
        >
          <strong style={{ fontSize: "1.1rem" }}>AlphaMintX</strong>
          <nav style={{ display: "flex", gap: "1rem", fontSize: "0.9rem" }}>
            <a href="/" style={{ color: "#0a5bd3", textDecoration: "none" }}>
              Dashboard
            </a>
            <a href="/strategies" style={{ color: "#0a5bd3", textDecoration: "none" }}>
              Strategies
            </a>
            <a href="/reasoning" style={{ color: "#0a5bd3", textDecoration: "none" }}>
              Reasoning viewer
            </a>
          </nav>
        </header>
        <main style={{ maxWidth: "56rem", margin: "0 auto", padding: "1.5rem" }}>
          {children}
        </main>
      </body>
    </html>
  );
}
