"use client";

// Public marketing landing (the dashboard moved to /dashboard): a SaaS
// front door on the same globals.css token system. Copy lives in the i18n
// catalog (en/vi); the CTAs route to /login and /signup.

import Link from "next/link";

import { useI18n } from "../src/lib/i18n";
import { PrefsToggles } from "./prefs";

const FEATURES = [
  { key: "plane", tone: "badge-accent", tag: "CORE" },
  { key: "ladder", tone: "badge-green", tag: "L0\u2013L3" },
  { key: "kill", tone: "badge-red", tag: "SAFETY" },
  { key: "limits", tone: "badge-yellow", tag: "ADMIN" },
  { key: "approvals", tone: "badge-cyan", tag: "L1" },
  { key: "record", tone: "badge-neutral", tag: "AUDIT" },
] as const;

export default function LandingPage() {
  const { t } = useI18n();
  return (
    <div className="landing">
      <header className="landing-nav">
        <span className="landing-brand">
          <span className="logo">A</span>
          AlphaMintX
        </span>
        <nav className="row">
          <PrefsToggles />
          <Link href="/login" className="btn btn-ghost">
            {t("landing.signin")}
          </Link>
          <Link href="/signup" className="btn btn-primary">
            {t("landing.getstarted")}
          </Link>
        </nav>
      </header>

      <section className="hero">
        <span className="badge badge-accent">
          <span className="dot" />
          {t("landing.badge")}
        </span>
        <h1 className="hero-title">{t("landing.title")}</h1>
        <p className="hero-sub">{t("landing.sub")}</p>
        <div className="hero-cta">
          <Link href="/signup" className="btn btn-primary">
            {t("landing.cta.create")}
          </Link>
          <Link href="/login" className="btn">
            {t("landing.signin")}
          </Link>
        </div>
      </section>

      <section className="landing-section">
        <div className="grid grid-3">
          {FEATURES.map(({ key, tone, tag }) => (
            <div className="card" key={key}>
              <h3 className="card-title row">
                <span className={`badge ${tone}`}>
                  <span className="dot" />
                  {tag}
                </span>
                {t(`feature.${key}.title`)}
              </h3>
              <p className="small muted">{t(`feature.${key}.detail`)}</p>
            </div>
          ))}
        </div>
      </section>

      <footer className="landing-foot">{t("landing.foot")}</footer>
    </div>
  );
}
