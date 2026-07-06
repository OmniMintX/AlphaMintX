"use client";

// Platform settings (platform_admin only): write-only exchange + LLM
// credentials. GET /platform/secrets serves METADATA only (env/base_url, key
// last4, updated_at/by) — stored values are never displayed again. A
// non-admin session 403s (and an unprovisioned vault 503s VAULT_UNAVAILABLE);
// both render as the error banner instead of the cards.

import { useState } from "react";

import {
  fetchPlatformSecrets,
  setBinanceSecret,
  setLlmSecret,
} from "../../src/lib/api/client";
import type { BinanceEnv, PlatformSecret } from "../../src/lib/api/schema";
import { usePoll } from "../../src/lib/api/usePoll";
import { useI18n } from "../../src/lib/i18n";
import { ErrorBanner } from "../strategies/ui";

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// "2026-07-04T12:00:00Z" -> "2026-07-04 12:00" (UTC, deterministic).
function fmtTime(iso: string): string {
  return iso.slice(0, 16).replace("T", " ");
}

function findSecret<K extends PlatformSecret["kind"]>(
  items: PlatformSecret[] | undefined,
  kind: K,
): Extract<PlatformSecret, { kind: K }> | undefined {
  return items?.find((s): s is Extract<PlatformSecret, { kind: K }> => s.kind === kind);
}

export default function SettingsPage() {
  const { t } = useI18n();
  const secrets = usePoll(fetchPlatformSecrets);
  const binance = findSecret(secrets.data?.items, "binance");
  const llm = findSecret(secrets.data?.items, "llm");

  return (
    <>
      <div className="page-head">
        <h1 className="page-title">{t("settings.title")}</h1>
        <p className="page-sub">{t("settings.sub")}</p>
      </div>
      {secrets.error && <ErrorBanner message={secrets.error} />}
      {!secrets.data && !secrets.error && (
        <div className="grid grid-2">
          <div className="skeleton" style={{ height: 200 }} />
          <div className="skeleton" style={{ height: 200 }} />
        </div>
      )}
      {secrets.data && (
        <div className="grid grid-2">
          <BinanceCard current={binance} onSaved={secrets.refresh} />
          <LlmCard current={llm} onSaved={secrets.refresh} />
        </div>
      )}
    </>
  );
}

function BinanceCard({
  current,
  onSaved,
}: {
  current: Extract<PlatformSecret, { kind: "binance" }> | undefined;
  onSaved: () => void;
}) {
  const { t } = useI18n();
  const [env, setEnv] = useState<BinanceEnv>("testnet");
  const [apiKey, setApiKey] = useState("");
  const [apiSecret, setApiSecret] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const submit = async () => {
    setPending(true);
    setError(null);
    setSaved(false);
    try {
      await setBinanceSecret(env, apiKey, apiSecret);
      setApiKey("");
      setApiSecret("");
      setSaved(true);
      onSaved();
    } catch (err) {
      setError(errText(err));
    } finally {
      setPending(false);
    }
  };

  return (
    <div className="card">
      <h3 className="card-title">{t("settings.binance.title")}</h3>
      {error && <ErrorBanner message={error} />}
      {saved && <p className="faint small">{t("settings.saved")}</p>}
      <p className="small muted">
        {current
          ? t("settings.binance.configured", {
              env: current.meta.env,
              last4: current.meta.api_key_last4,
              time: fmtTime(current.updated_at),
              by: current.updated_by,
            })
          : t("settings.notconfigured")}
      </p>
      <div className="row">
        <label className="field">
          <span className="field-label">{t("settings.env")}</span>
          <select
            className="select"
            value={env}
            onChange={(e) => setEnv(e.target.value as BinanceEnv)}
          >
            <option value="testnet">testnet</option>
            <option value="prod">prod</option>
          </select>
        </label>
        <label className="field">
          <span className="field-label">{t("settings.apikey")}</span>
          <input
            className="input"
            type="password"
            autoComplete="off"
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
          />
        </label>
        <label className="field">
          <span className="field-label">{t("settings.apisecret")}</span>
          <input
            className="input"
            type="password"
            autoComplete="off"
            value={apiSecret}
            onChange={(e) => setApiSecret(e.target.value)}
          />
        </label>
      </div>
      {env === "prod" && (
        <div className="banner banner-warn" style={{ marginTop: 10 }}>
          <span aria-hidden>&#9888;</span>
          <span>{t("settings.prodwarn")}</span>
        </div>
      )}
      <div className="row" style={{ marginTop: 10 }}>
        <button
          type="button"
          className="btn btn-primary"
          disabled={pending || apiKey.trim() === "" || apiSecret.trim() === ""}
          onClick={() => void submit()}
        >
          {t("settings.save")}
        </button>
        <span className="faint small">{t("settings.writeonly")}</span>
      </div>
    </div>
  );
}

function LlmCard({
  current,
  onSaved,
}: {
  current: Extract<PlatformSecret, { kind: "llm" }> | undefined;
  onSaved: () => void;
}) {
  const { t } = useI18n();
  const [baseUrl, setBaseUrl] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [timeoutSecs, setTimeoutSecs] = useState("30");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const timeoutNum = Number(timeoutSecs.trim());
  const timeoutValid = Number.isInteger(timeoutNum) && timeoutNum >= 1;

  const submit = async () => {
    setPending(true);
    setError(null);
    setSaved(false);
    try {
      await setLlmSecret(baseUrl.trim(), apiKey, timeoutNum);
      setApiKey("");
      setSaved(true);
      onSaved();
    } catch (err) {
      setError(errText(err));
    } finally {
      setPending(false);
    }
  };

  return (
    <div className="card">
      <h3 className="card-title">{t("settings.llm.title")}</h3>
      {error && <ErrorBanner message={error} />}
      {saved && <p className="faint small">{t("settings.saved")}</p>}
      <p className="small muted">
        {current
          ? t("settings.llm.configured", {
              base_url: current.meta.base_url,
              last4: current.meta.api_key_last4,
              timeout: current.meta.timeout_seconds,
              time: fmtTime(current.updated_at),
              by: current.updated_by,
            })
          : t("settings.notconfigured")}
      </p>
      <div className="row">
        <label className="field">
          <span className="field-label">{t("settings.baseurl")}</span>
          <input
            className="input"
            style={{ minWidth: "16rem" }}
            placeholder="https://api.openai.com/v1"
            value={baseUrl}
            onChange={(e) => setBaseUrl(e.target.value)}
          />
        </label>
        <label className="field">
          <span className="field-label">{t("settings.apikey")}</span>
          <input
            className="input"
            type="password"
            autoComplete="off"
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
          />
        </label>
        <label className="field">
          <span className="field-label">{t("settings.timeout")}</span>
          <input
            className="input"
            type="number"
            step={1}
            min={1}
            value={timeoutSecs}
            onChange={(e) => setTimeoutSecs(e.target.value)}
          />
        </label>
      </div>
      <div className="row" style={{ marginTop: 10 }}>
        <button
          type="button"
          className="btn btn-primary"
          disabled={pending || baseUrl.trim() === "" || apiKey.trim() === "" || !timeoutValid}
          onClick={() => void submit()}
        >
          {t("settings.save")}
        </button>
        <span className="faint small">{t("settings.writeonly")}</span>
      </div>
    </div>
  );
}
