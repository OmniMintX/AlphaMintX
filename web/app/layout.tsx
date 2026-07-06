import type { Metadata } from "next";
import type { ReactNode } from "react";

import { LocaleProvider } from "../src/lib/i18n";
import "./globals.css";
import { AppShell } from "./shell";

export const metadata: Metadata = {
  title: "AlphaMintX",
  description:
    "LLM-driven auto trading: dashboard, reasoning viewer, copilot approvals, kill-switch controls.",
};

// Pre-paint preference bootstrap: applies the persisted theme and locale to
// <html> before hydration (no flash). SSR always renders lang="en"/dark;
// suppressHydrationWarning covers the attribute delta and the LocaleProvider
// syncs React state from the DOM in an effect.
const PREFS_SCRIPT = `try{var t=localStorage.getItem("amx-theme");if(t==="light")document.documentElement.dataset.theme="light";var l=localStorage.getItem("amx-locale");if(l!=="en"&&l!=="vi")l=((navigator.language||"").toLowerCase().indexOf("vi")===0)?"vi":"en";document.documentElement.lang=l;}catch(e){}`;

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: PREFS_SCRIPT }} />
      </head>
      <body>
        <LocaleProvider>
          <AppShell>{children}</AppShell>
        </LocaleProvider>
      </body>
    </html>
  );
}
