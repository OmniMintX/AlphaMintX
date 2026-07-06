"use client";

// Platform settings (platform_admin only): write-only exchange + LLM
// credentials. GET /platform/secrets serves METADATA only (env/base_url, key
// last4, updated_at/by) — stored values are never displayed again. A
// non-admin session 403s (and an unprovisioned vault 503s VAULT_UNAVAILABLE);
// both render as the error banner instead of the cards.

import { useEffect, useRef, useState } from "react";

import {
  fetchPlatformSecrets,
  setBinanceSecret,
  setLlmSecret,
} from "../../src/lib/api/client";
import type { BinanceEnv, PlatformSecret } from "../../src/lib/api/schema";
import { usePoll } from "../../src/lib/api/usePoll";
import { useI18n } from "../../src/lib/i18n";
import { ErrorBanner } from "../strategies/ui";

// Models the agent-plane can price (llm/prices.json), offered as datalist
// suggestions — any other model string is allowed and metered as an
// estimated $0 (docs/specs/llm-routing-and-budget.md §3).
const LLM_MODELS = ["gpt-4o", "gpt-4o-mini"] as const;
const DEFAULT_TRADER_MODEL = "gpt-4o";
const DEFAULT_ANALYST_MODEL = "gpt-4o-mini";
const MODEL_MAX_LEN = 128; // control-plane caps model names at 128 chars.

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
        <div className="grid grid-2" role="status" aria-busy="true">
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

  // Seed the form from the stored metadata once (poll refreshes must not
  // clobber in-progress edits).
  const seeded = useRef(false);
  useEffect(() => {
    if (!current || seeded.current) return;
    seeded.current = true;
    setEnv(current.meta.env);
  }, [current]);

  // Keys are write-only: with a stored secret, leaving BOTH blank keeps it.
  const bothKeys = apiKey.trim() !== "" && apiSecret.trim() !== "";
  const keepKeys = current !== undefined && apiKey.trim() === "" && apiSecret.trim() === "";

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
        <label className="field" htmlFor="binance-api-key">
          <span className="field-label">{t("settings.apikey")}</span>
          <input
            id="binance-api-key"
            className="input"
            type="password"
            autoComplete="off"
            placeholder={current ? t("settings.keepkey") : undefined}
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
          />
        </label>
        <label className="field" htmlFor="binance-api-secret">
          <span className="field-label">{t("settings.apisecret")}</span>
          <input
            id="binance-api-secret"
            className="input"
            type="password"
            autoComplete="off"
            placeholder={current ? t("settings.keepkey") : undefined}
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
          disabled={pending || !(bothKeys || keepKeys)}
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
  const [traderModel, setTraderModel] = useState(DEFAULT_TRADER_MODEL);
  const [defaultModel, setDefaultModel] = useState(DEFAULT_ANALYST_MODEL);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  // Seed the form from the stored metadata once (poll refreshes must not
  // clobber in-progress edits). The key stays blank: blank keeps it.
  const seeded = useRef(false);
  useEffect(() => {
    if (!current || seeded.current) return;
    seeded.current = true;
    setBaseUrl(current.meta.base_url);
    setTimeoutSecs(String(current.meta.timeout_seconds));
    setTraderModel(current.meta.trader_model ?? DEFAULT_TRADER_MODEL);
    setDefaultModel(current.meta.default_model ?? DEFAULT_ANALYST_MODEL);
  }, [current]);

  const timeoutNum = Number(timeoutSecs.trim());
  const timeoutValid = Number.isInteger(timeoutNum) && timeoutNum >= 1;
  const keyOk = apiKey.trim() !== "" || current !== undefined;
  const traderTrim = traderModel.trim();
  const defaultTrim = defaultModel.trim();
  const modelsValid =
    traderTrim !== "" &&
    traderTrim.length <= MODEL_MAX_LEN &&
    defaultTrim !== "" &&
    defaultTrim.length <= MODEL_MAX_LEN;

  const submit = async () => {
    setPending(true);
    setError(null);
    setSaved(false);
    try {
      await setLlmSecret(baseUrl.trim(), apiKey, timeoutNum, traderTrim, defaultTrim);
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
              trader_model: current.meta.trader_model ?? DEFAULT_TRADER_MODEL,
              default_model: current.meta.default_model ?? DEFAULT_ANALYST_MODEL,
              time: fmtTime(current.updated_at),
              by: current.updated_by,
            })
          : t("settings.notconfigured")}
      </p>
      <div className="row">
        <label className="field" htmlFor="llm-base-url">
          <span className="field-label">{t("settings.baseurl")}</span>
          <input
            id="llm-base-url"
            className="input"
            style={{ minWidth: "16rem" }}
            placeholder="https://api.openai.com/v1"
            value={baseUrl}
            onChange={(e) => setBaseUrl(e.target.value)}
          />
        </label>
        <label className="field" htmlFor="llm-api-key">
          <span className="field-label">{t("settings.apikey")}</span>
          <input
            id="llm-api-key"
            className="input"
            type="password"
            autoComplete="off"
            placeholder={current ? t("settings.keepkey") : undefined}
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
          />
        </label>
        <label className="field" htmlFor="llm-timeout">
          <span className="field-label">{t("settings.timeout")}</span>
          <input
            id="llm-timeout"
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
        <label className="field" htmlFor="llm-trader-model">
          <span className="field-label">{t("settings.llm.trader_model")}</span>
          <input
            id="llm-trader-model"
            className="input"
            list="llm-model-suggestions"
            value={traderModel}
            onChange={(e) => setTraderModel(e.target.value)}
          />
        </label>
        <label className="field" htmlFor="llm-default-model">
          <span className="field-label">{t("settings.llm.default_model")}</span>
          <input
            id="llm-default-model"
            className="input"
            list="llm-model-suggestions"
            value={defaultModel}
            onChange={(e) => setDefaultModel(e.target.value)}
          />
        </label>
        <datalist id="llm-model-suggestions">
          {LLM_MODELS.map((m) => (
            <option key={m} value={m} />
          ))}
        </datalist>
      </div>
      <p className="faint small">{t("settings.llm.model_hint")}</p>
      <div className="row" style={{ marginTop: 10 }}>
        <button
          type="button"
          className="btn btn-primary"
          disabled={pending || baseUrl.trim() === "" || !keyOk || !timeoutValid || !modelsValid}
          onClick={() => void submit()}
        >
          {t("settings.save")}
        </button>
        <span className="faint small">{t("settings.writeonly")}</span>
      </div>
    </div>
  );
}
