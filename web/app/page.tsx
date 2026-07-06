// Public marketing landing (the dashboard moved to /dashboard): a dark
// SaaS front door on the same globals.css token system. Server-rendered,
// no data fetches — the CTAs route to /login and /signup.

import Link from "next/link";

const FEATURES = [
  {
    title: "Plane boundary",
    tone: "badge-accent",
    tag: "CORE",
    detail:
      "LLMs never place orders. Models propose; the deterministic Risk Gate and the Go OMS dispose \u2014 every order, every time.",
  },
  {
    title: "Autonomy ladder",
    tone: "badge-green",
    tag: "L0\u2013L3",
    detail:
      "Per-strategy ladder from advisor to full-auto. Promotion to real money requires a code-enforced paper-gate, not a checkbox.",
  },
  {
    title: "Kill-switch tiers",
    tone: "badge-red",
    tag: "SAFETY",
    detail:
      "Strategy, tenant, and platform kills cancel entries, preserve protective stops, and never auto-restart.",
  },
  {
    title: "Human risk limits",
    tone: "badge-yellow",
    tag: "ADMIN",
    detail:
      "Limits are a hard ceiling set by humans \u2014 neither trader users nor AI agents can raise them at runtime.",
  },
  {
    title: "Copilot approvals",
    tone: "badge-cyan",
    tag: "L1",
    detail:
      "Per-proposal human approval with a hard timeout: no decision means auto-reject, never a silent submit.",
  },
  {
    title: "Immutable record",
    tone: "badge-neutral",
    tag: "AUDIT",
    detail:
      "Append-only track record and full reasoning traces \u2014 identical strategy code across backtest, paper, and live.",
  },
] as const;

export default function LandingPage() {
  return (
    <div className="landing">
      <header className="landing-nav">
        <span className="landing-brand">
          <span className="logo">A</span>
          AlphaMintX
        </span>
        <nav className="row">
          <Link href="/login" className="btn btn-ghost">
            Sign in
          </Link>
          <Link href="/signup" className="btn btn-primary">
            Get started
          </Link>
        </nav>
      </header>

      <section className="hero">
        <span className="badge badge-accent">
          <span className="dot" />
          LLM-driven trading, human-governed
        </span>
        <h1 className="hero-title">
          Autonomous trading with deterministic guardrails
        </h1>
        <p className="hero-sub">
          AlphaMintX runs LLM strategy agents behind a hard plane boundary:
          models propose, the deterministic Risk Gate and OMS dispose. Climb
          the autonomy ladder from advisor to full-auto &mdash; with
          kill-switches at every tier and an append-only audit trail.
        </p>
        <div className="hero-cta">
          <Link href="/signup" className="btn btn-primary">
            Create a workspace
          </Link>
          <Link href="/login" className="btn">
            Sign in
          </Link>
        </div>
      </section>

      <section className="landing-section">
        <div className="grid grid-3">
          {FEATURES.map(({ title, tone, tag, detail }) => (
            <div className="card" key={title}>
              <h3 className="card-title row">
                <span className={`badge ${tone}`}>
                  <span className="dot" />
                  {tag}
                </span>
                {title}
              </h3>
              <p className="small muted">{detail}</p>
            </div>
          ))}
        </div>
      </section>

      <footer className="landing-foot">
        plane boundary enforced &mdash; LLMs never touch orders
      </footer>
    </div>
  );
}
