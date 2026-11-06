import { describe, expect, test } from 'bun:test'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRouter, RouterProvider } from '@tanstack/react-router'
import { renderToString, renderToStaticMarkup } from 'react-dom/server'
import type { ReactNode } from 'react'
import { createKnowledgeApi } from '../api/knowledge'
import type { Config, IngestJob } from '../api/types'
import { collectionsQuery, configQuery, documentsQuery, esaConnectionQuery, releaseQuery, sourcesQuery } from '../api/queries'
import { createMockKnowledge, MOCK_SYNC_MS, MOCK_SYNC_QUEUED_MS } from '../dev/mock-knowledge'
import { Hits, ResultSummary } from '../features/search/search-page'
import { SourcesList } from '../features/sources/sources-page'
import { I18nProvider } from '../i18n'
import { messages } from '../copy'
import { routeTree } from './pages'

type RenderOptions = {
  platformManaged?: boolean
  esaConnectionError?: boolean
  llm?: boolean
  embedding?: Config['embedding']
  /** `/config` reports a running re-embed backfill. */
  backfill?: boolean
  /** After the first syncs finish, esa syncs again (halfway through) and Notion waits behind it. */
  syncInFlight?: boolean
}
async function renderRoute(path: string, language: 'ja' | 'en', { platformManaged = false, esaConnectionError = false, llm = true, embedding, backfill = false, syncInFlight = false }: RenderOptions = {}) {
  let now = Date.parse('2026-10-15T00:00:00Z')
  const handle = createMockKnowledge({ platformManaged, esaPlatformManaged: platformManaged, esaConnectionError, llm, backfill, clock: () => now })
  const api = createKnowledgeApi((input, init) => handle(new Request(new URL(input, 'http://knowledge.test'), init)))
  const source = await api.sources.create({ type: 'notion', collection: 'handbook', config: { ...(platformManaged ? {} : { token: 'write-only-secret' }), rootPageIds: ['page-demo', 'error:missing'] } })
  await api.sources.sync(source.id)
  const esa = await api.sources.create({ type: 'esa', collection: 'handbook', config: { ...(platformManaged ? {} : { token: 'esa-write-only-secret' }), team: 'docs-team', query: 'in:Engineering error:missing' } })
  await api.sources.sync(esa.id)
  // The mock worker runs the two jobs one after the other.
  now += 2 * MOCK_SYNC_MS
  if (syncInFlight) {
    await api.sources.sync(esa.id)
    await api.sources.sync(source.id)
    now += MOCK_SYNC_QUEUED_MS + (MOCK_SYNC_MS - MOCK_SYNC_QUEUED_MS) / 2
  }
  if (backfill) {
    // The countdown starts at the first read; 3 s later one batch of 8 is re-embedded.
    await api.config()
    now += 3_000
  }
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } })
  client.setQueryData(collectionsQuery.queryKey, await api.collections.list())
  const config = await api.config()
  client.setQueryData(configQuery.queryKey, { ...config, embedding: embedding ?? config.embedding })
  client.setQueryData(releaseQuery.queryKey, await api.release())
  client.setQueryData(documentsQuery('handbook').queryKey, await api.documents.list('handbook'))
  client.setQueryData(sourcesQuery.queryKey, await api.sources.list())
  client.setQueryData(esaConnectionQuery.queryKey, await api.providers.esa())
  const router = createRouter({ routeTree, context: { queryClient: client }, history: createMemoryHistory({ initialEntries: [path] }) })
  await router.load()
  const html = renderToString(<QueryClientProvider client={client}><I18nProvider language={language}><RouterProvider router={router} /></I18nProvider></QueryClientProvider>)
  client.clear()
  return html
}

describe('Knowledge route screens with mock API data', () => {
  test('header carries the app switcher in the current language', async () => {
    expect(await renderRoute('/', 'en')).toContain('aria-label="Switch app"')
    expect(await renderRoute('/', 'ja')).toContain('aria-label="アプリを切り替える"')
  })
  test('footer and search summary share localized embedding sources and show the configured LLM', async () => {
    for (const language of ['en', 'ja'] as const) {
      const copy = (id: keyof typeof messages) => language === 'ja' ? messages[id].translations.ja : messages[id].defaultMessage
      for (const source of ['embedding', 'openai', 'custom', 'hashing'] as const) {
        const embedding = { provider: source === 'hashing' ? 'hashing' : 'openai', source, dimensions: 768, backfill: { pending: 0, state: 'idle' as const } }
        const label = copy(`knowledge.embeddingSource.${source}`)
        const llmLabel = copy('knowledge.llmProvider').replace('{provider}', copy('knowledge.llmGateway'))
        const html = (await renderRoute('/', language, { embedding })).replaceAll('<!-- -->', '')
        expect(html).toContain(`<footer><span>${label} · 768d · ${llmLabel}</span>`)
        const result = { hits: [], mode: 'hybrid' as const, reranked: false, expanded: [], embedding }
        const summary = renderToStaticMarkup(<I18nProvider language={language}><ResultSummary result={result} /></I18nProvider>)
        const expected = copy('knowledge.results').replace('{count}', '0').replace('{mode}', copy('knowledge.hybrid')).replace('{provider}', label)
        expect(summary).toContain(`<p class="result-summary" role="status">${expected}</p>`)
      }
      const withoutLLM = (await renderRoute('/', language, { llm: false })).replaceAll('<!-- -->', '')
      expect(withoutLLM).toContain(`<footer><span>${copy('knowledge.embeddingSource.hashing')} · 256d</span>`)
    }
  })
  test('collections route renders collection, visibility and create form', async () => {
    const html = await renderRoute('/collections', 'en')
    expect(html).toContain('Create collection')
    expect(html).toContain('handbook')
    expect(html).toContain('Everyone in this project')
    expect(html).toContain('pj-knowledge-demo')
    expect(html).toContain('mock-knowledg')
  })
  test('documents route renders documents, chunk counts and Markdown input', async () => {
    const html = await renderRoute('/documents', 'en')
    expect(html).toContain('Welcome to Knowledge')
    expect(html).toContain('1 chunks')
    expect(html).toContain('Markdown content')
  })
  test('sources route renders sync counts/errors and never the saved token', async () => {
    const html = await renderRoute('/sources', 'en')
    expect(html).toContain('Add source')
    expect(html).toContain('1 documents · 1 chunks')
    expect(html).toContain('Mock Notion page unavailable: error:missing')
    expect(html).not.toContain('write-only-secret')
    expect(html).toContain('type="password"')
  })
  test('sources route renders the esa form fields and esa source rows', async () => {
    const html = await renderRoute('/sources', 'en')
    expect(html).toContain('Source type')
    expect(html).toContain('<option value="esa">esa</option>')
    expect(html).toContain('esa access token')
    expect(html).toContain('https://&lt;team&gt;.esa.io/user/applications')
    expect(html.match(/type="password"/g)).toHaveLength(2)
    // Notion is the default type: the esa branch is rendered but hidden and disabled (not validated or submitted).
    expect(html).toContain('<option value="notion" selected="">Notion</option>')
    expect(html).toContain('<fieldset hidden="" disabled=""><div class="notice info">')
    expect(html).toContain('<label class="field"><span>esa access token')
    expect(html).toContain('esa team')
    expect(html).toMatch(/<input required="" pattern="\[a-z0-9\]\[a-z0-9\\-\]\*"[^>]* name="team"\/>/)
    expect(html).toContain('esa search query')
    expect(html).toContain('in:Engineering tag:api')
    expect(html).toContain('<input type="checkbox" name="includeWip"/>')
    expect(html).toContain('WIP posts are drafts')
    expect(html).toContain('esa · docs-team')
    expect(html).toMatch(/esa search query(<!-- -->)?: (<!-- -->)?in:Engineering error:missing/)
    expect(html).toContain('<input type="checkbox" name="includeArchived"/>')
    expect(html).toContain('esa&#x27;s Archived category is excluded unless checked.')
    expect(html).toMatch(/WIP posts: excluded(<!-- -->)? · (<!-- -->)?Archived posts: excluded/)
    expect(html).toContain('2 documents · 2 chunks')
    expect(html).toContain('Mock esa post unavailable: in:Engineering error:missing')
    expect(html).not.toContain('esa-write-only-secret')
  })
  test('sources route shows a running sync with progress beside the last sync, and a queued one waiting', async () => {
    const html = await renderRoute('/sources', 'en', { syncInFlight: true })
    const esaRow = html.slice(html.indexOf('esa · docs-team'))
    const notionRow = html.slice(html.indexOf('Notion · handbook'), html.indexOf('esa · docs-team'))
    // esa: halfway through its two posts, the button is busy and the previous result stays.
    expect(esaRow).toMatch(/<button type="button" class="secondary" disabled="">Syncing…<\/button>/)
    expect(esaRow).toMatch(/<div class="sync-status" aria-live="polite"><div class="sync-job"><p>Syncing: (<!-- -->)?1(<!-- -->)? of (<!-- -->)?2(<!-- -->)? documents · (<!-- -->)?1(<!-- -->)? chunks<\/p><progress aria-label="Sync progress" max="2" value="1"><\/progress><\/div><\/div>/)
    expect(esaRow).toContain('2 documents · 2 chunks')
    // Notion waits behind esa on the single worker: text only, no bar yet.
    expect(notionRow).toContain('<div class="sync-status" aria-live="polite"><div class="sync-job"><p>Waiting to sync…</p></div></div>')
    expect(notionRow).toMatch(/<button type="button" class="secondary" disabled="">Syncing…<\/button>/)
    expect(notionRow).toContain('1 documents · 1 chunks')
    // Finished syncs show only the last-sync block.
    const settled = await renderRoute('/sources', 'en')
    expect(settled).not.toContain('<progress')
    expect(settled).not.toContain('Syncing')
    expect(settled.match(/<button type="button" class="secondary">Sync now<\/button>/g)).toHaveLength(2)
    expect(await renderRoute('/sources', 'ja', { syncInFlight: true })).toMatch(/同期中: (<!-- -->)?2(<!-- -->)?件中(<!-- -->)?1(<!-- -->)?ドキュメント/)
  })
  test('a Notion sync without a total counts up, and a failed sync shows its error over the last result', () => {
    const job = { id: 'job-1', kind: 'sync' as const, createdAt: '2026-10-15T00:00:00Z', startedAt: '2026-10-15T00:00:01Z', result: null, error: null, finishedAt: null }
    const source = { id: 'src-1', type: 'notion' as const, collection: 'handbook', config: { rootPageIds: ['page-1'] }, lastSync: { documents: 4, chunks: 9, removed: 0, errors: [], finishedAt: '2026-10-14T00:00:00Z' } }
    const render = (sync: IngestJob) => renderToStaticMarkup(<I18nProvider language="en"><SourcesList sources={[{ ...source, sync }]} onSync={() => {}} onRemove={() => {}} /></I18nProvider>)
    const running = render({ ...job, state: 'running', progress: { documents: 3, chunks: 7, total: null } })
    expect(running).toContain('<p>Syncing: 3 documents · 7 chunks so far</p><progress aria-label="Sync progress"></progress>')
    const failed = render({ ...job, state: 'failed', progress: { documents: 0, chunks: 0, total: null }, error: 'Notion returned HTTP 401', finishedAt: '2026-10-15T00:00:02Z' })
    expect(failed).toContain('<div class="notice error"><strong>The latest sync failed</strong><p>Notion returned HTTP 401</p><p>Documents imported earlier stay searchable. Fix the cause, then sync again.</p></div>')
    expect(failed).toContain('4 documents · 9 chunks')
    expect(failed).toContain('<button type="button" class="secondary">Sync now</button>')
    expect(failed).not.toContain('<progress')
  })
  test('managed sources hide token input and link to the installed app settings in both languages', async () => {
    for (const language of ['en', 'ja'] as const) {
      const html = await renderRoute('/sources', language, { platformManaged: true })
      expect(html).toContain(language === 'en' ? 'Notion token is managed in the Project&#x27;s app settings' : 'Notionトークンはプロジェクトのアプリ設定で管理されています')
      expect(html).toContain(language === 'en' ? 'esa token is managed in the Project&#x27;s app settings' : 'esaトークンはプロジェクトのアプリ設定で管理されています')
      expect(html.match(/href="https:\/\/dashboard\.appthrust\.test\/dashboard\/projects\/pj-knowledge-demo\/marketplace\/knowledge\/credentials"/g)).toHaveLength(2)
      expect(html).toContain(language === 'en' ? 'esa team' : 'esaチーム')
      expect(html).not.toContain('type="password"')
      expect(html).not.toContain('write-only-secret')
    }
  })
  test('esa connection: not connected links to settings, connected lists teams as a select, rejected shows the error', async () => {
    const settingsLink = /href="https:\/\/dashboard\.appthrust\.test\/dashboard\/projects\/pj-knowledge-demo\/marketplace\/knowledge\/credentials"/g
    const unmanaged = await renderRoute('/sources', 'en')
    expect(unmanaged).toContain('esa.io is not connected')
    expect(unmanaged).toMatch(/<a href="https:\/\/dashboard\.appthrust\.test\/dashboard\/projects\/pj-knowledge-demo\/marketplace\/knowledge\/credentials">Connect esa.io in the Project&#x27;s app settings<\/a>/)
    expect(unmanaged).not.toMatch(/<select[^>]* name="team"/)

    const connected = await renderRoute('/sources', 'en', { platformManaged: true })
    expect(connected).toContain('<div class="notice success" role="status"><strong>Connected to esa.io</strong>')
    expect(connected).toContain('esa.io official documents')
    expect(connected).toContain('<img src="https://img.esa.io/mock/docs.png" alt=""')
    expect(connected).toMatch(/<select[^>]* name="team"[^>]*><option value="docs" selected="">docs<\/option><option value="engineering">engineering<\/option><\/select>/)
    expect(connected).not.toMatch(/<input[^>]* name="team"/)
    expect(connected).not.toContain('esa.io is not connected')

    for (const language of ['en', 'ja'] as const) {
      const rejected = await renderRoute('/sources', language, { platformManaged: true, esaConnectionError: true })
      expect(rejected).toContain(language === 'en' ? 'Could not connect to esa.io' : 'esa.ioに接続できません')
      expect(rejected).toContain('esa returned HTTP 401')
      expect(rejected).toContain(language === 'en' ? 'Replace the esa token in the Project&#x27;s app settings' : 'プロジェクトのアプリ設定でesaトークンを差し替える')
      expect(rejected.match(settingsLink)).toHaveLength(3)
      expect(rejected).toMatch(/<input required="" pattern="[^"]*"[^>]* name="team"\/>/)
      expect(rejected).not.toMatch(/<select[^>]* name="team"/)
    }
  })
  test('search route renders mode, collection filters, topK and configured Ask', async () => {
    const html = await renderRoute('/', 'en')
    expect(html).toContain('Search playground')
    expect(html).toContain('Maximum hits')
    expect(html).toContain('value="8"')
    expect(html).toContain('Hybrid')
    expect(html).toContain('handbook')
    expect(html).toContain('Citations arrive before')
  })
  test('search route offers LLM rerank only when an LLM is configured', async () => {
    const configured = await renderRoute('/', 'en')
    expect(configured).toContain('<input type="checkbox" name="rerank"/>Rerank with LLM')
    expect(configured).toContain('Re-orders the top results with the configured LLM; slower, more precise.')
    const unconfigured = await renderRoute('/', 'en', { llm: false })
    expect(unconfigured).not.toContain('name="rerank"')
    expect(unconfigured).not.toContain('Rerank with LLM')
    expect(unconfigured).not.toContain('name="expand"')
    expect(unconfigured).not.toContain('Citations arrive before')
  })
  test('search route offers LLM query expansion beside rerank, and expanded formulations render as chips', async () => {
    const html = await renderRoute('/', 'en')
    expect(html).toMatch(/<div class="search-options"><label class="field"><span class="check"><input type="checkbox" name="rerank"\/>Rerank with LLM<\/span>.*<\/label><label class="field"><span class="check"><input type="checkbox" name="expand"\/>Expand query with LLM<\/span><small>Adds official terms and rephrasings from the LLM; one extra call.<\/small><\/label><\/div>/)
    const handle = createMockKnowledge()
    const api = createKnowledgeApi((input, init) => handle(new Request(new URL(input, 'http://knowledge.test'), init)))
    const expanded = await api.search({ query: '有給', expand: true })
    const render = (language: 'en' | 'ja', node: ReactNode) => renderToStaticMarkup(<I18nProvider language={language}>{node}</I18nProvider>)
    const chips = render('en', <ResultSummary result={expanded} />)
    expect(chips).toMatch(/<div class="expanded"><span class="muted" id="([^"]+)">Also searched for<\/span><ul class="chips" aria-labelledby="\1"><li class="chip">年次有給休暇<\/li><li class="chip">休暇申請<\/li><\/ul><\/div>/)
    expect(render('ja', <ResultSummary result={expanded} />)).toContain('あわせて検索した語句')
    expect(render('en', <ResultSummary result={await api.search({ query: '有給' })} />)).not.toContain('chips')
  })
  test('a running re-embed backfill shows a banner on every route; none when idle', async () => {
    for (const path of ['/', '/collections', '/documents', '/sources']) {
      const html = await renderRoute(path, 'en', { backfill: true })
      expect(html).toMatch(/<div class="backfill-banner" role="status"><p><span class="pulse" aria-hidden="true"><\/span>Re-embedding documents for the new embedding model \((<!-- -->)?40(<!-- -->)? remaining\)…<\/p><\/div><main/)
    }
    expect(await renderRoute('/', 'ja', { backfill: true })).toMatch(/新しい埋め込みモデル向けにドキュメントを再埋め込みしています（残り(<!-- -->)?40(<!-- -->)?件）…/)
    expect(await renderRoute('/', 'en')).toContain('<div class="backfill-banner" role="status"></div><main')
  })
  test('Japanese copy renders on every screen', async () => {
    for (const [path, expected] of [['/', '検索プレイグラウンド'], ['/', 'LLMで並べ替える'], ['/', 'LLMでクエリを補う'], ['/collections', 'コレクションを作成'], ['/documents', 'Markdown本文'], ['/sources', 'ソースを追加'], ['/sources', 'esaアクセストークン'], ['/sources', 'WIP記事を含める'], ['/sources', 'WIP記事: 含めない'], ['/sources', 'アーカイブ済みの記事を含める'], ['/sources', 'アーカイブ済みの記事: 含めない']]) {
      expect(await renderRoute(path, 'ja')).toContain(expected)
    }
    for (const message of Object.values(messages)) expect(message.translations.ja.length).toBeGreaterThan(0)
  })
  test('matched chunks render score/identity and escape document HTML and unsafe URLs', async () => {
    const handle = createMockKnowledge()
    const api = createKnowledgeApi((input, init) => handle(new Request(new URL(input, 'http://knowledge.test'), init)))
    await api.documents.create({ collection: 'handbook', title: '<script>bad</script>', url: 'javascript:alert(1)', content: 'quasar <script>alert(1)</script>' })
    const result = await api.search({ query: 'quasar', mode: 'lexical' })
    const html = renderToStaticMarkup(<I18nProvider language="en"><Hits hits={result.hits} /></I18nProvider>)
    expect(html).toContain('Chunk 0')
    expect(html).toContain('Score 1.00000')
    expect(html).toContain('&lt;script&gt;')
    expect(html).not.toContain('href="javascript:')
    expect(html).not.toContain('<script>')
  })
  test('reranked results show a badge and esa hits show update date and watchers', async () => {
    let now = 0
    const handle = createMockKnowledge({ clock: () => now })
    const api = createKnowledgeApi((input, init) => handle(new Request(new URL(input, 'http://knowledge.test'), init)))
    const source = await api.sources.create({ type: 'esa', collection: 'handbook', config: { token: 'secret', team: 'docs-team' } })
    await api.sources.sync(source.id)
    now += MOCK_SYNC_MS
    const result = await api.search({ query: 'Onboarding', mode: 'vector', rerank: true })
    const render = (node: ReactNode) => renderToStaticMarkup(<I18nProvider language="en">{node}</I18nProvider>)
    expect(render(<ResultSummary result={result} />)).toContain('<span class="badge">Reranked by LLM</span>')
    expect(render(<ResultSummary result={{ ...result, reranked: false }} />)).not.toContain('badge')
    const hits = render(<Hits hits={result.hits} />)
    expect(hits).toMatch(/<time datetime="2026-09-30T10:15:00\+09:00">Updated 2026-09-30<\/time> · 9 watchers/i)
    expect(hits).toContain('Updated 2026-10-08')
    expect(hits.match(/watchers/g)).toHaveLength(2)
  })
})
