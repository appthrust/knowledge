import { AppSwitcher } from '@appthrust/app-launcher'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { configQuery, releaseQuery } from '../api/queries'
import { embeddingSourceLabel } from '../copy'
import { setLanguage, useText } from '../i18n'
import { ExternalLink, QueryFeedback } from '../ui'
import { BackfillBanner } from './backfill-banner'

export function Shell({ children }: { children: ReactNode }) {
  const { t, language } = useText()
  const config = useQuery(configQuery)
  const release = useQuery(releaseQuery)
  return <div className="app-shell">
    <a href="#main" className="skip-link">{t('knowledge.skip')}</a>
    <header>
      <div className="topbar"><Link to="/" className="brand">{t('knowledge.product')}</Link>
        {config.data && <div className="project-identity"><span className="mono">{config.data.projectId}</span><ExternalLink url={config.data.publicUrl}>{config.data.publicUrl}</ExternalLink></div>}
        <button type="button" className="secondary language" aria-label={t('knowledge.language')} onClick={() => setLanguage(language === 'ja' ? 'en' : 'ja')}><span lang={language === 'ja' ? 'en' : 'ja'}>{language === 'ja' ? 'English' : '日本語'}</span></button>
        <AppSwitcher language={language} />
      </div>
      <nav aria-label={t('knowledge.nav')}>
        <Link to="/" activeOptions={{ exact: true }}>{t('knowledge.search')}</Link>
        <Link to="/collections">{t('knowledge.collections')}</Link>
        <Link to="/documents">{t('knowledge.documents')}</Link>
        <Link to="/sources">{t('knowledge.sources')}</Link>
      </nav>
    </header>
    <BackfillBanner />
    <main id="main" tabIndex={-1}>
      <QueryFeedback pending={config.isPending} error={config.error} retry={config.refetch} />
      {children}
    </main>
    <footer>
      {config.data && <span>{embeddingSourceLabel(config.data.embedding.source, t)} · {config.data.embedding.dimensions}d{config.data.llm.configured && <> · {t('knowledge.llmProvider', { provider: config.data.llm.provider === 'gateway' ? t('knowledge.llmGateway') : config.data.llm.provider || '—' })}</>}</span>}
      {release.data ? <span className="mono" title={JSON.stringify(release.data)}>{t('knowledge.release')}: {release.data.releaseVersion || '—'} · {release.data.sourceRevision?.slice(0, 12) || '—'}</span> : <span>{t(release.isPending ? 'knowledge.loading' : 'knowledge.releaseUnavailable')}</span>}
    </footer>
  </div>
}
