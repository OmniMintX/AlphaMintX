"use client";

// Locale (EN/VI) + theme (dark/light) segmented toggles. Rendered in the
// sidebar (app shell) and on the public landing/auth surfaces.

import { useI18n, useTheme } from "../src/lib/i18n";

export function PrefsToggles() {
  const { t, locale, setLocale } = useI18n();
  const { theme, setTheme } = useTheme();
  return (
    <div className="prefs-row">
      <div className="seg" role="group" aria-label="Language">
        <button
          type="button"
          className={`seg-btn${locale === "en" ? " active" : ""}`}
          onClick={() => setLocale("en")}
        >
          EN
        </button>
        <button
          type="button"
          className={`seg-btn${locale === "vi" ? " active" : ""}`}
          onClick={() => setLocale("vi")}
        >
          VI
        </button>
      </div>
      <div className="seg" role="group" aria-label="Theme">
        <button
          type="button"
          className={`seg-btn${theme === "dark" ? " active" : ""}`}
          onClick={() => setTheme("dark")}
        >
          {t("prefs.theme.dark")}
        </button>
        <button
          type="button"
          className={`seg-btn${theme === "light" ? " active" : ""}`}
          onClick={() => setTheme("light")}
        >
          {t("prefs.theme.light")}
        </button>
      </div>
    </div>
  );
}
