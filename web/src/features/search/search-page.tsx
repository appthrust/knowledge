import { useMutation, useQuery } from '@tanstack/react-query'
import { useEffect, useId, useRef, useState } from 'react'
import { knowledgeApi } from '../../api/knowledge'
import { collectionsQuery, configQuery } from '../../api/queries'
import type { AnswerInput, SearchHit, SearchMode, SearchResult } from '../../api/types'
import { embeddingSourceLabel } from '../../copy'
import { useText } from '../../i18n'
import { ErrorNotice, ExternalLink, Field, PageHeading, QueryFeedback } from '../../ui'

export function SearchPage() {
  const { t } = useText()
  const collections = useQuery(collectionsQuery)
  const config = useQuery(configQuery)
  const [query, setQuery] = useState('')
  const [selected, setSelected] = useState<string[]>([])
  const [mode, setMode] = useState<SearchMode>('hybrid')
  const [topK, setTopK] = useState(8)
  const [rerank, setRerank] = useState(false)
  const [expand, setExpand] = useState(false)
  const llmConfigured = config.data?.llm.configured === true
  const search = useMutation({ mutationFn: knowledgeApi.search })
  return <>
    <PageHeading title={t('knowledge.search')}>{t('knowledge.searchHelp')}</PageHeading>
    <form className="search-form" onSubmit={(event) => { event.preventDefault(); search.mutate({ query, collections: selected, mode, topK, ...(llmConfigured ? { rerank, expand } : {}) }) }}>
      <Field label={t('knowledge.query')}><input name="query" value={query} required onChange={(event) => setQuery(event.target.value)} /></Field>
      <div className="search-controls">
        <Field label={t('knowledge.mode')}><select value={mode} onChange={(event) => setMode(event.target.value as SearchMode)}>{(['hybrid', 'lexical', 'vector'] as const).map((value) => <option key={value} value={value}>{t(`knowledge.${value}`)}</option>)}</select></Field>
        <Field label={t('knowledge.topK')}><input type="number" required min={1} max={50} value={topK} onChange={(event) => setTopK(event.target.valueAsNumber)} /></Field>
        <button disabled={search.isPending || !query.trim()}>{t(search.isPending ? 'knowledge.loading' : 'knowledge.runSearch')}</button>
      </div>
      {llmConfigured && <div className="search-options">
        <label className="field"><span className="check"><input name="rerank" type="checkbox" checked={rerank} onChange={(event) => setRerank(event.target.checked)} />{t('knowledge.rerank')}</span><small>{t('knowledge.rerankHelp')}</small></label>
        <label className="field"><span className="check"><input name="expand" type="checkbox" checked={expand} onChange={(event) => setExpand(event.target.checked)} />{t('knowledge.expand')}</span><small>{t('knowledge.expandHelp')}</small></label>
      </div>}
      <fieldset><legend>{t('knowledge.filter')}</legend>
        <QueryFeedback pending={collections.isPending} error={collections.error} retry={collections.refetch} />
        <div className="collection-filters">{collections.data?.map((collection) => <label key={collection.name}><input type="checkbox" checked={selected.includes(collection.name)} onChange={(event) => setSelected((current) => event.target.checked ? [...current, collection.name] : current.filter((name) => name !== collection.name))} />{collection.name}</label>)}</div>
        <small>{t('knowledge.allCollections')}</small>
      </fieldset>
    </form>
    <ErrorNotice error={search.error} />
    {search.data ? <section aria-label={t('knowledge.search')}>
      <ResultSummary result={search.data} />
      <Hits hits={search.data.hits} />
    </section> : <p className="empty">{t('knowledge.beforeSearch')}</p>}
    <QueryFeedback pending={config.isPending} error={config.error} retry={config.refetch} />
    {llmConfigured && <AskPanel input={{ query, collections: selected }} />}
  </>
}

export function ResultSummary({ result }: { result: SearchResult }) {
  const { t } = useText()
  const expandedLabel = useId()
  return <>
    <p className="result-summary" role="status">{t('knowledge.results', { count: result.hits.length, mode: t(`knowledge.${result.mode}`), provider: embeddingSourceLabel(result.embedding.source, t) })}{result.reranked && <span className="badge">{t('knowledge.reranked')}</span>}</p>
    {result.expanded.length > 0 && <div className="expanded"><span className="muted" id={expandedLabel}>{t('knowledge.expanded')}</span><ul className="chips" aria-labelledby={expandedLabel}>{result.expanded.map((text) => <li key={text} className="chip">{text}</li>)}</ul></div>}
  </>
}

export function Hits({ hits }: { hits: SearchHit[] }) {
  const { t } = useText()
  if (!hits.length) return <p className="empty">{t('knowledge.noHits')}</p>
  return <ol className="hits">{hits.map((hit, rank) => <li key={`${hit.documentId}:${hit.chunkIndex}`}>
    <span className="rank" aria-hidden>{rank + 1}</span>
    <div className="hit-content">
      <div className="row"><h2><ExternalLink url={hit.url}>{hit.title}</ExternalLink></h2><span className="score">{t('knowledge.score', { score: hit.score.toFixed(5) })}</span></div>
      <p className="muted mono">{hit.collection} · {t('knowledge.chunk', { index: hit.chunkIndex })} · {hit.documentId}</p>
      <HitMetadata metadata={hit.metadata} />
      <pre className="chunk-text">{hit.text}</pre>
    </div>
  </li>)}</ol>
}

/** Connector metadata worth a glance (esa: last update date, watcher count); everything else stays out of the list. */
function HitMetadata({ metadata }: { metadata?: Record<string, unknown> }) {
  const { t } = useText()
  const updatedAt = typeof metadata?.updatedAt === 'string' && /^\d{4}-\d{2}-\d{2}/.test(metadata.updatedAt) ? metadata.updatedAt : undefined
  const watchers = typeof metadata?.watchers === 'number' && Number.isFinite(metadata.watchers) ? metadata.watchers : undefined
  if (updatedAt === undefined && watchers === undefined) return null
  // The date is shown as written by the source (its own offset), so it never shifts with the viewer's time zone.
  return <p className="muted">
    {updatedAt && <time dateTime={updatedAt}>{t('knowledge.updatedAt', { date: updatedAt.slice(0, 10) })}</time>}
    {updatedAt && watchers !== undefined && ' · '}
    {watchers !== undefined && t('knowledge.watchers', { count: watchers })}
  </p>
}

export function AskPanel({ input }: { input: AnswerInput }) {
  const { t } = useText()
  const active = useRef<AbortController | null>(null)
  const [citations, setCitations] = useState<SearchHit[]>([])
  const [text, setText] = useState('')
  const [status, setStatus] = useState<'idle' | 'streaming' | 'done' | 'stopped'>('idle')
  const [error, setError] = useState<unknown>(null)
  useEffect(() => () => { active.current?.abort() }, [])
  const ask = async () => {
    active.current?.abort()
    const controller = new AbortController()
    active.current = controller
    setCitations([])
    setText('')
    setError(null)
    setStatus('streaming')
    try {
      await knowledgeApi.answer(input, (event) => {
        if (controller.signal.aborted) return
        if (event.type === 'citation') setCitations((current) => [...current, event.data])
        if (event.type === 'token') setText((current) => current + event.data.text)
        if (event.type === 'done') setStatus('done')
      }, controller.signal)
    } catch (failure) {
      if (!controller.signal.aborted) { setError(failure); setStatus('idle') }
    } finally {
      if (active.current === controller) active.current = null
    }
  }
  return <section className="ask-panel" aria-label={t('knowledge.ask')}>
    <h2>{t('knowledge.ask')}</h2><p className="muted">{t('knowledge.askHelp')}</p>
    <div className="actions"><button type="button" disabled={status === 'streaming' || !input.query.trim()} onClick={() => { void ask() }}>{t('knowledge.ask')}</button>
      {status === 'streaming' && <button type="button" className="secondary" onClick={() => { active.current?.abort(); setStatus('stopped') }}>{t('knowledge.cancel')}</button>}
    </div>
    <ErrorNotice error={error} />
    {citations.length > 0 && <><h3>{t('knowledge.citations')}</h3><Hits hits={citations} /></>}
    {text && <><h3>{t('knowledge.answer')}</h3><p className="answer-text" aria-live="polite">{text}</p></>}
    {status !== 'idle' && <p role="status">{t(status === 'done' ? 'knowledge.answerDone' : status === 'stopped' ? 'knowledge.cancelled' : 'knowledge.loading')}</p>}
  </section>
}
