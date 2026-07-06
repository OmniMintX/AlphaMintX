"use client";

// First-run bootstrap: POST /api/auth/bootstrap creates the platform admin
// (the control plane 409s once one exists — that CONFLICT surfaces
// verbatim), then the same credentials sign in and land on /dashboard.

import Link from "next/link";
import { useState, type FormEvent } from "react";

import { bootstrap, login } from "../../src/lib/api/client";
import { AuthCard, authErrText } from "../auth-card";

export default function BootstrapPage() {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setPending(true);
    setError(null);
    try {
      await bootstrap(email, password);
    } catch (err) {
      setError(authErrText(err));
      setPending(false);
      return;
    }
    try {
      await login(email, password);
      window.location.href = "/dashboard";
    } catch {
      window.location.href = "/login";
    }
  }

  return (
    <AuthCard
      title="Bootstrap platform admin"
      sub="One-time first-run setup — creates the platform admin account."
      foot={
        <>
          Already bootstrapped? <Link href="/login">Sign in</Link>
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
            autoComplete="new-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
        <button type="submit" className="btn btn-primary auth-submit" disabled={pending}>
          {pending ? "Bootstrapping\u2026" : "Create admin"}
        </button>
      </form>
    </AuthCard>
  );
}
