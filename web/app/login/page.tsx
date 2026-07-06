"use client";

// Sign-in: POST /api/auth/login sets the HttpOnly session cookie
// server-side, then a hard navigation lands on /dashboard. A 401
// INVALID_CREDENTIALS (or any upstream error) surfaces verbatim.

import Link from "next/link";
import { useState, type FormEvent } from "react";

import { login } from "../../src/lib/api/client";
import { AuthCard, authErrText } from "../auth-card";

export default function LoginPage() {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setPending(true);
    setError(null);
    try {
      await login(email, password);
      window.location.href = "/dashboard";
    } catch (err) {
      setError(authErrText(err));
      setPending(false);
    }
  }

  return (
    <AuthCard
      title="Sign in"
      sub="Session-based access — your token never reaches the browser."
      foot={
        <>
          No account? <Link href="/signup">Create a workspace</Link>
          <span className="faint"> · </span>
          <Link href="/bootstrap">First-run bootstrap</Link>
        </>
      }
    >
      <form className="auth-form" onSubmit={onSubmit}>
        {error && <div className="banner banner-error">{error}</div>}
        <label className="field">
          <span className="field-label">Email</span>
          <input
            className="input"
            type="email"
            autoComplete="email"
            required
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
        </label>
        <label className="field">
          <span className="field-label">Password</span>
          <input
            className="input"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
        <button type="submit" className="btn btn-primary auth-submit" disabled={pending}>
          {pending ? "Signing in\u2026" : "Sign in"}
        </button>
      </form>
    </AuthCard>
  );
}
