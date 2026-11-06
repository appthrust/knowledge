// Display language and the typed translator over the knowledge catalogs
// (src/copy). The choice is stored in the same `appthrust-language` cookie the
// console and Dashboard use, so a person keeps one choice per site.
// Precedence: cookie → ja (the knowledge's audience and default wording).
import {
  dashboardLocale,
  DashboardI18nProvider,
  useDashboardI18n,
  type DashboardLanguage,
  type DashboardMessageValues,
} from '@appthrust/platform-dashboard-i18n'
import { Store, useSelector } from '@tanstack/react-store'
import type { ReactNode } from 'react'
import { catalogs, type MessageId } from './copy'

export const LANGUAGES: readonly DashboardLanguage[] = ['ja', 'en']

const COOKIE_NAME = 'appthrust-language'
const COOKIE_MAX_AGE = 60 * 60 * 24 * 365

export function isLanguage(value: unknown): value is DashboardLanguage {
  return value === 'ja' || value === 'en'
}

/** The saved choice, else Japanese. Browser language is not consulted. */
export function resolveInitialLanguage(cookie: string): DashboardLanguage {
  for (const part of cookie.split(';')) {
    const [name, ...rest] = part.trim().split('=')
    if (name !== COOKIE_NAME) continue
    const value = decodeURIComponent(rest.join('='))
    return isLanguage(value) ? value : 'ja'
  }
  return 'ja'
}

const languageStore = new Store<{ language: DashboardLanguage }>({
  language: typeof document === 'undefined' ? 'ja' : resolveInitialLanguage(document.cookie),
})

/** Persists the choice (cookie, one year) and updates `<html lang>`. */
export function setLanguage(language: DashboardLanguage) {
  if (typeof document !== 'undefined') {
    document.cookie = `${COOKIE_NAME}=${language}; Max-Age=${COOKIE_MAX_AGE}; Path=/; SameSite=Lax`
    document.documentElement.lang = language
  }
  languageStore.setState((state) => (state.language === language ? state : { language }))
}

export function useLanguage(): DashboardLanguage {
  return useSelector(languageStore, (state) => state.language)
}

export function I18nProvider({ language, children }: { language: DashboardLanguage; children: ReactNode }) {
  return (
    <DashboardI18nProvider language={language} catalogs={catalogs}>
      {children}
    </DashboardI18nProvider>
  )
}

export type Translate = (id: MessageId, values?: DashboardMessageValues) => string

/** The current language's translator; message ids are checked at compile time. */
export function useText(): { t: Translate; language: DashboardLanguage; locale: string } {
  const i18n = useDashboardI18n()
  return { t: i18n.t, language: i18n.language, locale: dashboardLocale(i18n.language) }
}

if (typeof document !== 'undefined') document.documentElement.lang = languageStore.state.language
