"use client";

// Tenant signup: POST /api/auth/signup creates the tenant + owner user
// (409 EMAIL_TAKEN surfaces verbatim), then the same credentials sign in to
// set the session cookie and land on /dashboard. If that follow-up login
// fails, fall back to /login rather than stranding the new account.

import Link from "next/link";
import { useState, type FormEvent } from "react";

import { login, signup } from "../../src/lib/api/client";
import { useI18n } from "../../src/lib/i18n";
import { AuthCard, authErrText } from "../auth-card";

export default function SignupPage() {
  const { t } = useI18n();
  const [tenantName, setTenantName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setPending(true);
    setError(null);
    try {
      await signup(tenantName, email, password);
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
      title={t("signup.title")}
      sub={t("signup.sub")}
      foot={
        <>
          {t("signup.have")} <Link href="/login">{t("login.title")}</Link>
        </>
      }
    >
      <form className="auth-form" onSubmit={onSubmit}>
        {error && <div className="banner banner-error">{error}</div>}
        <label className="field">
          <span className="field-label">{t("signup.workspace")}</span>
          <input
            className="input"
            type="text"
            autoComplete="organization"
            required
            value={tenantName}
            onChange={(e) => setTenantName(e.target.value)}
          />
        </label>
        <label className="field">
          <span className="field-label">{t("auth.email")}</span>
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
          <span className="field-label">{t("auth.password")}</span>
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
          {pending ? t("signup.pending") : t("signup.title")}
        </button>
      </form>
    </AuthCard>
  );
}
