import { describe, expect, test } from 'bun:test'
import { createMockKnowledge, MOCK_EXPANDED, MOCK_SYNC_MS, MOCK_SYNC_QUEUED_MS, type MockKnowledgeOptions } from '../dev/mock-knowledge'
import { consumeAnswer, createKnowledgeApi, isActiveBackfill, isActiveJob, type KnowledgeApi, KnowledgeApiError, parseConfig, parseJob, parseSearchResult } from './knowledge'
import type { AnswerEvent, SearchHit } from './types'

/** The mock on a test clock: `advance` moves sync jobs and the backfill countdown along. */
function mockApi(options: MockKnowledgeOptions = {}) {
  let now = Date.parse('2026-10-15T00:00:00Z')
  const handle = createMockKnowledge({ ...options, clock: () => now })
  const calls: { input: string; init: RequestInit }[] = []
  const api = createKnowledgeApi((input, init) => {
    calls.push({ input, init })
    return handle(new Request(new URL(input, 'http://knowledge.test'), init))
  })
  return { api, calls, handle, advance: (ms: number) => { now += ms } }
}
/** Enqueues a sync, lets the mock worker finish it and returns its result. */
async function syncToCompletion({ api, advance }: { api: KnowledgeApi; advance: (ms: number) => void }, id: string) {
  const job = await api.sources.sync(id)
  advance(MOCK_SYNC_MS)
  const done = await api.jobs.get(job.id)
  expect(done.state).toBe('done')
  return done.result
}
const hit: SearchHit = { documentId: 'doc-1', title: 'Handbook', url: '', collection: 'handbook', chunkIndex: 2, text: '検索できるチャンク', score: 0.03226 }
function eventStream(text: string, fragmentBytes = 7) {
  const encoded = new TextEncoder().encode(text)
  let cursor = 0
  return new Response(new ReadableStream<Uint8Array>({ pull(controller) {
    if (cursor === encoded.length) { controller.close(); return }
    controller.enqueue(encoded.slice(cursor, cursor += Math.min(fragmentBytes, encoded.length - cursor)))
  } }), { headers: { 'Content-Type': 'text/event-stream; charset=utf-8' } })
}

describe('Knowledge API client and in-memory contract', () => {
  test('parses search hits and preserves ranking, chunk identity, score, provider and source', async () => {
    const { api, calls } = mockApi()
    const result = await api.search({ query: 'hybrid retrieval', collections: ['handbook'], mode: 'hybrid', topK: 3 })
    expect(result.mode).toBe('hybrid')
    expect(result.embedding).toEqual({ provider: 'hashing', source: 'hashing' })
    expect(result.hits).toHaveLength(1)
    expect(result.hits[0]).toMatchObject({ title: 'Welcome to Knowledge', collection: 'handbook', chunkIndex: 0 })
    expect(result.hits[0].text).toContain('hybrid retrieval')
    expect(result.hits[0].score).toBeGreaterThan(0)
    expect(calls[0].input).toBe('/api/knowledge/v1/search')
    expect(JSON.parse(String(calls[0].init.body))).toEqual({ query: 'hybrid retrieval', collections: ['handbook'], mode: 'hybrid', topK: 3 })
    expect(calls[0].init.credentials).toBe('same-origin')
    expect(new Headers(calls[0].init.headers).has('authorization')).toBe(false)
  })
  test('parses embedding display sources and falls back to the protocol on older servers', async () => {
    const config = await mockApi().api.config()
    expect(config.embedding).toMatchObject({ provider: 'hashing', source: 'hashing', dimensions: 256 })
    expect(config.llm).toEqual({ configured: true, provider: 'gateway' })
    const result = { hits: [], mode: 'hybrid', embedding: { provider: 'openai', source: 'embedding' } }
    expect(parseSearchResult(result).embedding).toEqual({ provider: 'openai', source: 'embedding' })
    expect(parseSearchResult({ ...result, embedding: { provider: 'openai' } }).embedding).toEqual({ provider: 'openai', source: 'openai' })
    expect(parseConfig({ ...config, embedding: { ...config.embedding, ...result.embedding } }).embedding.source).toBe('embedding')
    const legacy = parseConfig({ ...config, embedding: { provider: 'openai', dimensions: 768, backfill: config.embedding.backfill }, llm: { configured: true } })
    expect(legacy.embedding.source).toBe('openai')
    expect(legacy.llm.provider).toBeUndefined()
  })
  test('rejects malformed search responses instead of showing plausible empty results', () => {
    expect(() => parseSearchResult({ hits: [{ ...hit, score: '0.5' }], mode: 'hybrid', embedding: { provider: 'hashing' } })).toThrow('Invalid search hit: score')
    expect(() => parseSearchResult({ hits: [{ ...hit, chunkIndex: -1 }], mode: 'hybrid', embedding: { provider: 'hashing' } })).toThrow('Invalid search hit: chunkIndex')
    expect(() => parseSearchResult({ mode: 'hybrid' })).toThrow('Invalid search response: hits')
    expect(() => parseSearchResult({ hits: [{ ...hit, metadata: ['watchers'] }], mode: 'hybrid', embedding: { provider: 'hashing' } })).toThrow('Invalid search hit: metadata')
    expect(() => parseSearchResult({ hits: [], mode: 'hybrid', reranked: 'yes', embedding: { provider: 'hashing' } })).toThrow('Invalid search response: reranked')
  })
  test('lists, creates and deletes collections and upserts documents by externalId', async () => {
    const { api } = mockApi()
    await api.collections.create({ name: 'engineering', description: 'Build notes', visibility: { groups: ['g-engineering'] } })
    expect((await api.collections.list()).find((value) => value.name === 'engineering')?.visibility.groups).toEqual(['g-engineering'])
    const input = { collection: 'engineering', externalId: 'design', title: 'Design', content: 'old content' }
    const first = await api.documents.create(input)
    const second = await api.documents.create({ ...input, content: 'quasar '.repeat(400) })
    expect(second.id).toBe(first.id)
    expect(second.chunks).toBe(3)
    expect((await api.documents.get(second.id)).content).toContain('quasar')
    expect(await api.documents.list('engineering')).toHaveLength(1)
    expect((await api.search({ query: 'quasar', collections: ['handbook'], mode: 'lexical' })).hits).toHaveLength(0)
    expect((await api.search({ query: 'quasar', collections: ['engineering'], mode: 'lexical', topK: 1 })).hits).toHaveLength(1)
    await api.documents.remove(second.id)
    expect(await api.documents.list('engineering')).toEqual([])
    await api.collections.remove('engineering')
    expect((await api.collections.list()).map((value) => value.name)).toEqual(['handbook'])
  })
  test('Notion tokens are write-only and sync status includes partial errors', async () => {
    const mock = mockApi()
    const { api } = mock
    const source = await api.sources.create({ type: 'notion', collection: 'handbook', config: { token: 'never-return-this', rootPageIds: ['page-1', 'error:missing'] } })
    expect(JSON.stringify(source)).not.toContain('never-return-this')
    const result = await syncToCompletion(mock, source.id)
    expect(result).toEqual({ documents: 1, chunks: 1, removed: 0, errors: ['Mock Notion page unavailable: error:missing'] })
    const saved = await api.sources.get(source.id)
    expect(saved.lastSync?.documents).toBe(1)
    expect(saved.lastSync?.finishedAt).toBeString()
    expect(JSON.stringify(await api.sources.list())).not.toContain('never-return-this')
    await syncToCompletion(mock, source.id)
    expect((await api.documents.list('handbook')).filter((document) => document.externalId === `notion:${source.id}:page-1`)).toHaveLength(1)
  })
  test('managed Notion config rejects supplied tokens and creates token-free sources', async () => {
    const mock = mockApi({ platformManaged: true })
    const { api } = mock
    expect((await api.config()).notion.platformManaged).toBe(true)
    await expect(api.sources.create({ type: 'notion', collection: 'handbook', config: { token: 'never-return-this' } })).rejects.toMatchObject({ status: 400, code: 'knowledge_notion_token_platform_managed' })
    const source = await api.sources.create({ type: 'notion', collection: 'handbook', config: { rootPageIds: ['page-1'] } })
    expect(source.tokenSource).toBe('platform')
    expect((await api.sources.get(source.id)).tokenSource).toBe('platform')
    expect((await syncToCompletion(mock, source.id))?.documents).toBe(1)
    expect(JSON.stringify(await api.sources.list())).not.toContain('"token"')
    expect((await mockApi().api.config()).notion.platformManaged).toBe(false)
    await expect(mockApi().api.sources.create({ type: 'notion', collection: 'handbook', config: {} })).rejects.toMatchObject({ status: 400 })
  })
  test('esa sources send the contract body, keep tokens write-only and sync posts with partial errors', async () => {
    const mock = mockApi()
    const { api, calls } = mock
    const source = await api.sources.create({ type: 'esa', collection: 'handbook', config: { token: 'never-return-this', team: 'docs-team', query: 'in:Engineering error:missing', includeWip: false } })
    expect(JSON.parse(String(calls[0].init.body))).toEqual({ type: 'esa', collection: 'handbook', config: { token: 'never-return-this', team: 'docs-team', query: 'in:Engineering error:missing', includeWip: false } })
    expect(source).toMatchObject({ type: 'esa', tokenSource: 'stored', config: { team: 'docs-team', query: 'in:Engineering error:missing', includeWip: false } })
    expect(JSON.stringify(source)).not.toContain('never-return-this')
    expect(await syncToCompletion(mock, source.id)).toEqual({ documents: 2, chunks: 2, removed: 0, errors: ['Mock esa post unavailable: in:Engineering error:missing'] })
    const saved = await api.sources.get(source.id)
    expect(saved.lastSync?.documents).toBe(2)
    expect(JSON.stringify(await api.sources.list())).not.toContain('never-return-this')
    await syncToCompletion(mock, source.id)
    const posts = (await api.documents.list('handbook')).filter((document) => document.externalId?.startsWith(`esa:${source.id}:`))
    expect(posts.map((document) => [document.externalId, document.url]).sort()).toEqual([[`esa:${source.id}:1`, 'https://docs-team.esa.io/posts/1'], [`esa:${source.id}:2`, 'https://docs-team.esa.io/posts/2']])
    await expect(api.sources.create({ type: 'esa', collection: 'handbook', config: { token: 'x', team: 'Docs.Team' } })).rejects.toMatchObject({ status: 400, code: 'invalid_request' })
    await expect(api.sources.create({ type: 'esa', collection: 'handbook', config: { team: 'docs-team' } })).rejects.toMatchObject({ status: 400, code: 'invalid_request' })
  })
  test('esa includeArchived round-trips and defaults to excluded', async () => {
    const { api, calls } = mockApi()
    const archived = await api.sources.create({ type: 'esa', collection: 'handbook', config: { token: 'secret', team: 'docs-team', includeWip: false, includeArchived: true } })
    expect(JSON.parse(String(calls[0].init.body))).toEqual({ type: 'esa', collection: 'handbook', config: { token: 'secret', team: 'docs-team', includeWip: false, includeArchived: true } })
    expect(archived.config).toMatchObject({ includeWip: false, includeArchived: true })
    expect((await api.sources.get(archived.id)).config.includeArchived).toBe(true)
    const plain = await api.sources.create({ type: 'esa', collection: 'handbook', config: { token: 'secret', team: 'docs-team' } })
    expect(plain.config.includeArchived).toBe(false)
  })
  test('rerank flag round-trips, reorders the top hits, carries esa metadata and needs an LLM', async () => {
    const mock = mockApi()
    const { api, calls } = mock
    const source = await api.sources.create({ type: 'esa', collection: 'handbook', config: { token: 'secret', team: 'docs-team' } })
    await syncToCompletion(mock, source.id)
    calls.length = 0
    const plain = await api.search({ query: 'esa posts hybrid search', mode: 'vector' })
    const reranked = await api.search({ query: 'esa posts hybrid search', mode: 'vector', rerank: true })
    expect(JSON.parse(String(calls[0].init.body))).toEqual({ query: 'esa posts hybrid search', mode: 'vector' })
    expect(JSON.parse(String(calls[1].init.body))).toEqual({ query: 'esa posts hybrid search', mode: 'vector', rerank: true })
    expect(plain.reranked).toBe(false)
    expect(reranked.reranked).toBe(true)
    expect(plain.hits).toHaveLength(3)
    expect(reranked.hits.map((value) => value.documentId)).toEqual(plain.hits.map((value) => value.documentId).reverse())
    const esaHit = plain.hits.find((value) => value.title === 'Engineering/Handbook/Onboarding')
    expect(esaHit?.metadata).toMatchObject({ number: 1, updatedAt: '2026-09-30T10:15:00+09:00', watchers: 9 })
    expect(plain.hits.find((value) => value.title === 'Welcome to Knowledge')?.metadata).toBeUndefined()
    await expect(mockApi({ llm: false }).api.search({ query: 'handbook', rerank: true })).rejects.toMatchObject({ status: 503, code: 'knowledge_llm_unavailable' })
    expect((await mockApi({ llm: false }).api.search({ query: 'handbook', rerank: false })).reranked).toBe(false)
  })
  test('managed esa config rejects supplied tokens and creates token-free sources', async () => {
    const mock = mockApi({ esaPlatformManaged: true })
    const { api } = mock
    const config = await api.config()
    expect(config.esa.platformManaged).toBe(true)
    expect(config.notion.platformManaged).toBe(false)
    await expect(api.sources.create({ type: 'esa', collection: 'handbook', config: { token: 'never-return-this', team: 'docs-team' } })).rejects.toMatchObject({ status: 400, code: 'knowledge_esa_token_platform_managed' })
    const source = await api.sources.create({ type: 'esa', collection: 'handbook', config: { team: 'docs-team', includeWip: true } })
    expect(source).toMatchObject({ tokenSource: 'platform', config: { includeWip: true } })
    expect(await syncToCompletion(mock, source.id)).toEqual({ documents: 2, chunks: 2, removed: 0, errors: [] })
    expect(JSON.stringify(await api.sources.list())).not.toContain('"token"')
    expect((await mockApi().api.config()).esa.platformManaged).toBe(false)
  })
  test('esa provider connection reports not-managed, connected teams and rejected tokens', async () => {
    const connection = (options: Parameters<typeof createMockKnowledge>[0]) => {
      const handle = createMockKnowledge(options)
      const calls: string[] = []
      const api = createKnowledgeApi((input, init) => { calls.push(`${init.method} ${input}`); return handle(new Request(new URL(input, 'http://knowledge.test'), init)) })
      return { api, calls }
    }
    const unmanaged = connection({})
    expect(await unmanaged.api.providers.esa()).toEqual({ platformManaged: false, connected: false, teams: [] })
    expect(unmanaged.calls).toEqual(['GET /api/knowledge/v1/providers/esa'])
    const connected = await connection({ esaPlatformManaged: true }).api.providers.esa()
    expect(connected).toMatchObject({ platformManaged: true, connected: true })
    expect(connected.error).toBeUndefined()
    expect(connected.teams.map((team) => team.name)).toEqual(['docs', 'engineering'])
    expect(connected.teams[0]).toMatchObject({ description: 'esa.io official documents', icon: expect.stringMatching(/^https:\/\//) })
    const rejected = connection({ esaConnectionError: true }).api
    expect(await rejected.providers.esa()).toEqual({ platformManaged: true, connected: false, teams: [], error: 'esa returned HTTP 401' })
    expect((await rejected.config()).esa.platformManaged).toBe(true)
  })
  test('sync runs as a background job: 202 queued, running with progress, done with lastSync', async () => {
    const mock = mockApi()
    const { api, calls, handle, advance } = mock
    const source = await api.sources.create({ type: 'esa', collection: 'handbook', config: { token: 'secret', team: 'docs-team' } })
    expect(source.sync).toBeNull()
    calls.length = 0
    const queued = await api.sources.sync(source.id)
    expect(calls.map(({ input, init }) => [init.method, input, init.body])).toEqual([['POST', `/api/knowledge/v1/sources/${source.id}/sync`, undefined]])
    expect(queued).toMatchObject({ kind: 'sync', state: 'queued', progress: { documents: 0, chunks: 0, total: null }, result: null, error: null, startedAt: null, finishedAt: null })
    expect(isActiveJob(queued)).toBe(true)
    // Idempotent while in flight: the same job, again 202.
    const again = await handle(new Request(`http://knowledge.test/api/knowledge/v1/sources/${source.id}/sync`, { method: 'POST' }))
    expect(again.status).toBe(202)
    expect((await again.json() as { job: { id: string } }).job.id).toBe(queued.id)
    expect((await api.sources.list())[0].sync).toMatchObject({ id: queued.id, state: 'queued' })

    // Halfway through the run: one of esa's two posts is in, against a known total.
    advance(MOCK_SYNC_QUEUED_MS + (MOCK_SYNC_MS - MOCK_SYNC_QUEUED_MS) / 2)
    const running = await api.jobs.get(queued.id)
    expect(running).toMatchObject({ state: 'running', progress: { documents: 1, chunks: 1, total: 2 }, result: null })
    expect(running.startedAt).toBeString()
    const listed = await api.sources.get(source.id)
    expect(listed.sync?.state).toBe('running')
    expect(listed.lastSync).toBeNull()

    advance(MOCK_SYNC_MS)
    const done = await api.jobs.get(queued.id)
    expect(done).toMatchObject({ state: 'done', progress: { documents: 2, chunks: 2, total: 2 }, result: { documents: 2, chunks: 2, errors: [] }, error: null })
    expect(isActiveJob(done)).toBe(false)
    const synced = await api.sources.get(source.id)
    expect(synced.sync?.id).toBe(queued.id)
    expect(synced.lastSync).toEqual({ documents: 2, chunks: 2, removed: 0, errors: [], finishedAt: done.finishedAt as string })
    // Done: a new request enqueues a new job.
    expect((await api.sources.sync(source.id)).id).not.toBe(queued.id)
    await expect(api.jobs.get('missing')).rejects.toMatchObject({ status: 404, code: 'not_found' })
  })
  test('one worker runs sync jobs FIFO, Notion progress has no total and a failed walk keeps the last sync', async () => {
    const mock = mockApi()
    const { api, advance } = mock
    const notion = await api.sources.create({ type: 'notion', collection: 'handbook', config: { token: 'secret', rootPageIds: ['page-1', 'page-2'] } })
    const esa = await api.sources.create({ type: 'esa', collection: 'handbook', config: { token: 'secret', team: 'docs-team', query: 'fail:search' } })
    const first = await api.sources.sync(notion.id)
    const second = await api.sources.sync(esa.id)
    advance(MOCK_SYNC_MS - 1)
    expect((await api.jobs.get(first.id)).progress).toEqual({ documents: 1, chunks: 1, total: null })
    expect((await api.jobs.get(second.id)).state).toBe('queued')
    advance(1 + MOCK_SYNC_QUEUED_MS)
    expect((await api.jobs.get(first.id)).state).toBe('done')
    const failed = await api.jobs.get(second.id)
    expect(failed).toMatchObject({ state: 'failed', error: 'Mock esa search failed: fail:search', result: null })
    expect(failed.finishedAt).toBeString()
    expect((await api.sources.get(esa.id)).lastSync).toBeNull()
    expect((await api.sources.get(notion.id)).lastSync?.documents).toBe(2)
  })
  test('rejects malformed jobs', () => {
    const job = { id: 'job-1', kind: 'sync', state: 'running', progress: { documents: 1, chunks: 3, total: null }, result: null, error: null, createdAt: '2026-10-15T00:00:00Z', startedAt: '2026-10-15T00:00:01Z', finishedAt: null }
    expect(parseJob(job)).toEqual(job as ReturnType<typeof parseJob>)
    // A re-embed job counts documents only.
    expect(parseJob({ ...job, kind: 'reembed', progress: { documents: 8, total: 40 } }).progress).toEqual({ documents: 8, chunks: 0, total: 40 })
    expect(() => parseJob({ ...job, state: 'paused' })).toThrow('Invalid job: state')
    expect(() => parseJob({ ...job, kind: 'walk' })).toThrow('Invalid job: kind')
    expect(() => parseJob({ ...job, progress: { documents: -1, chunks: 0, total: null } })).toThrow('Invalid job: progress')
    expect(() => parseJob({ ...job, progress: { documents: 1, chunks: 0, total: '2' } })).toThrow('Invalid job: progress')
    expect(() => parseJob({ ...job, error: 500 })).toThrow('Invalid job: error')
  })
  test('expand flag round-trips, the expanded formulations come back and it needs an LLM', async () => {
    const { api, calls } = mockApi()
    const plain = await api.search({ query: '有給', mode: 'hybrid', rerank: false, expand: false })
    const expanded = await api.search({ query: '有給', mode: 'hybrid', rerank: true, expand: true })
    expect(JSON.parse(String(calls[0].init.body))).toEqual({ query: '有給', mode: 'hybrid', rerank: false, expand: false })
    expect(JSON.parse(String(calls[1].init.body))).toEqual({ query: '有給', mode: 'hybrid', rerank: true, expand: true })
    expect(plain.expanded).toEqual([])
    expect(expanded.expanded).toEqual(MOCK_EXPANDED)
    expect(expanded.expanded).toEqual(['年次有給休暇', '休暇申請'])
    await expect(mockApi({ llm: false }).api.search({ query: '有給', expand: true })).rejects.toMatchObject({ status: 503, code: 'knowledge_llm_unavailable' })
    await expect(api.search({ query: '有給', expand: 'yes' as unknown as boolean })).rejects.toMatchObject({ status: 400, code: 'invalid_request' })
    // The server sends [] (or null) without expand; anything but strings is rejected.
    const result = { hits: [], mode: 'hybrid', reranked: false, embedding: { provider: 'openai' } }
    expect(parseSearchResult({ ...result, expanded: null }).expanded).toEqual([])
    expect(parseSearchResult(result).expanded).toEqual([])
    expect(() => parseSearchResult({ ...result, expanded: [42] })).toThrow('Invalid search response: expanded')
    expect(() => parseSearchResult({ ...result, expanded: '年次有給休暇' })).toThrow('Invalid search response: expanded')
  })
  test('config reports the re-embed backfill: idle by default, a countdown with the backfill flag', async () => {
    expect((await mockApi().api.config()).embedding.backfill).toEqual({ pending: 0, state: 'idle' })
    const { api, advance } = mockApi({ backfill: true })
    const queued = (await api.config()).embedding.backfill
    expect(queued).toEqual({ pending: 48, state: 'queued' })
    expect(isActiveBackfill(queued)).toBe(true)
    advance(1_000 + 2_000)
    expect((await api.config()).embedding.backfill).toEqual({ pending: 40, state: 'running' })
    advance(60_000)
    const finished = (await api.config()).embedding.backfill
    expect(finished).toEqual({ pending: 0, state: 'idle' })
    expect(isActiveBackfill(finished)).toBe(false)
    expect(isActiveBackfill({ pending: 3, state: 'failed' })).toBe(false)
  })
  test('rejects a config without a well-formed backfill', () => {
    const config = { projectId: 'pj', publicUrl: 'https://knowledge.pj.example', embedding: { provider: 'openai', dimensions: 768, backfill: { pending: 3, state: 'running' } }, llm: { configured: true }, notion: { platformManaged: false }, esa: { platformManaged: false } }
    expect(parseConfig(config).embedding.backfill).toEqual({ pending: 3, state: 'running' })
    expect(() => parseConfig({ ...config, embedding: { provider: 'openai', dimensions: 768 } })).toThrow('Invalid knowledge API response')
    expect(() => parseConfig({ ...config, embedding: { ...config.embedding, backfill: { pending: 3, state: 'paused' } } })).toThrow('Invalid config: embedding.backfill')
    expect(() => parseConfig({ ...config, embedding: { ...config.embedding, backfill: { pending: 1.5, state: 'running' } } })).toThrow('Invalid config: embedding.backfill')
  })
  test('reads root config and release and keeps httpx error status/code', async () => {
    const { api } = mockApi({ llm: false })
    expect((await api.config()).llm).toEqual({ configured: false, provider: undefined })
    expect((await api.release()).releaseVersion).toBe('mock')
    try {
      await api.answer({ query: 'handbook' }, () => {})
      throw new Error('expected rejection')
    } catch (error) {
      expect(error).toBeInstanceOf(KnowledgeApiError)
      expect(error).toMatchObject({ status: 503, code: 'knowledge_llm_unavailable' })
    }
    await expect(api.search({ query: 'handbook', topK: 51 })).rejects.toMatchObject({ status: 400, code: 'invalid_request' })
  })
  test('mock answer emits all citations before tokens and done', async () => {
    const events: AnswerEvent[] = []
    await mockApi().api.answer({ query: 'hybrid' }, (event) => events.push(event))
    expect(events[0].type).toBe('citation')
    const firstToken = events.findIndex((event) => event.type === 'token')
    expect(events.slice(firstToken).some((event) => event.type === 'citation')).toBe(false)
    expect(events.at(-1)).toEqual({ type: 'done' })
  })
})

describe('answer SSE parsing', () => {
  test('handles single-byte UTF-8 fragments, CRLF, comments and multiline data', async () => {
    const events: AnswerEvent[] = []
    const response = eventStream(`: keepalive\r\nevent: citation\r\ndata: ${JSON.stringify(hit)}\r\n\r\nevent: token\r\ndata: {\r\ndata: "text":"回答です"}\r\n\r\nevent: done\r\ndata: {}\r\n\r\n`, 1)
    await consumeAnswer(response, (event) => events.push(event))
    expect(events).toEqual([{ type: 'citation', data: hit }, { type: 'token', data: { text: '回答です' } }, { type: 'done' }])
  })
  test('rejects truncated streams, malformed tokens and server error events', async () => {
    await expect(consumeAnswer(eventStream('event: token\ndata: {"text":"partial"}\n\n'), () => {})).rejects.toThrow('Answer stream ended before done')
    await expect(consumeAnswer(eventStream('event: token\ndata: {"text":42}\n\n'), () => {})).rejects.toThrow('Invalid answer token: text')
    await expect(consumeAnswer(eventStream('event: error\ndata: {"message":"provider disconnected"}\n\n'), () => {})).rejects.toThrow('provider disconnected')
    await expect(consumeAnswer(Response.json({}), () => {})).rejects.toThrow('expected event stream')
  })
  test('cancels the reader after done without waiting for server closure', async () => {
    let cancelled = false
    const stream = new ReadableStream<Uint8Array>({ start(controller) { controller.enqueue(new TextEncoder().encode('event: done\ndata: {}\n\n')) }, cancel() { cancelled = true } })
    await consumeAnswer(new Response(stream, { headers: { 'Content-Type': 'text/event-stream' } }), () => {})
    expect(cancelled).toBe(true)
  })
})
