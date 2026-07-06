"use client";

// Shared centered card for the public auth pages (/login, /signup,
// /bootstrap): brand mark, title/sub, the page's form, and a footer link
// row. Errors render as the standard banner-error inside the form.

import Link from "next/link";
import type { ReactNode } from "react";

export function AuthCard({
  title,
  sub,
  children,
  foot,
}: {
  title: string;
  sub: string;
  children: ReactNode;
  foot: ReactNode;
}) {
  return (
    <div className="auth-wrap">
      <div className="auth-card">
        <Link href="/" className="auth-brand">
          <span className="logo">A</span>
          AlphaMintX
        </Link>
        <h1 className="auth-title">{title}</h1>
        <p className="auth-sub">{sub}</p>
        {children}
        <div className="auth-foot">{foot}</div>
      </div>
    </div>
  );
}

export function authErrText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
