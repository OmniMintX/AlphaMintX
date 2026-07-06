"use client";

// Lightweight en/vi i18n + theme preferences. No library: a flat message
// catalog, a React context for the active locale, and localStorage-backed
// persistence. The pre-paint script in app/layout.tsx sets
// document.documentElement.lang (locale) and dataset.theme before hydration;
// the provider/hooks sync from the DOM in an effect so SSR markup (always
// "en"/dark) never mismatches during hydration.

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from "react";

import { messages, type Msg } from "./messages";
import { messagesApp } from "./messages-app";
import { messagesOps } from "./messages-ops";

export type Locale = "en" | "vi";
export type Theme = "dark" | "light";

export type MessageKey =
  | keyof typeof messages
  | keyof typeof messagesApp
  | keyof typeof messagesOps;

const ALL: Record<string, Msg> = { ...messages, ...messagesApp, ...messagesOps };

const LocaleCtx = createContext<{ locale: Locale; setLocale: (l: Locale) => void }>({
  locale: "en",
  setLocale: () => {},
});

export function LocaleProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<Locale>("en");

  useEffect(() => {
    if (document.documentElement.lang === "vi") setLocaleState("vi");
  }, []);

  const setLocale = useCallback((l: Locale) => {
    setLocaleState(l);
    document.documentElement.lang = l;
    try {
      localStorage.setItem("amx-locale", l);
    } catch {
      // private mode etc. — preference just won't persist
    }
  }, []);

  return <LocaleCtx.Provider value={{ locale, setLocale }}>{children}</LocaleCtx.Provider>;
}

export function useI18n() {
  const { locale, setLocale } = useContext(LocaleCtx);
  const t = useCallback(
    (key: MessageKey, params?: Record<string, string | number>): string => {
      const m = ALL[key];
      let s = m ? m[locale] : key;
      if (params) {
        for (const [k, v] of Object.entries(params)) s = s.replace(`{${k}}`, String(v));
      }
      return s;
    },
    [locale],
  );
  return { t, locale, setLocale };
}

export function useTheme() {
  const [theme, setThemeState] = useState<Theme>("dark");

  useEffect(() => {
    if (document.documentElement.dataset.theme === "light") setThemeState("light");
  }, []);

  const setTheme = useCallback((next: Theme) => {
    setThemeState(next);
    if (next === "light") document.documentElement.dataset.theme = "light";
    else delete document.documentElement.dataset.theme;
    try {
      localStorage.setItem("amx-theme", next);
    } catch {
      // preference just won't persist
    }
  }, []);

  return { theme, setTheme };
}
