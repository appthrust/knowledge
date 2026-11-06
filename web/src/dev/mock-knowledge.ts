import type { Collection, Config, DocumentInput, EmbeddingBackfill, EsaConnection, IngestJob, KnowledgeDocument, SearchHit, SearchInput, SearchMode, SearchResult, Source, SourceInput, SyncResult } from '../api/types'

const BASE = '/api/knowledge/v1'
const json = (body: unknown, status = 200) => Response.json(body, { status })
const fail = (status: number, error: string, message: string) => json({ error, message }, status)
const words = (text: string) => text.toLowerCase().match(/[\p{L}\p{N}]+/gu) ?? []

/** Dev-quality deterministic feature hashing; no network or model credentials. */
function embedding(text: string) {
  const vector = new Float64Array(256)
  const tokens = words(text)
  const features = [...tokens, ...tokens.slice(1).map((token, index) => `${tokens[index]} ${token}`)]
  for (const feature of features) {
    let hash = 2166136261
    for (const char of feature) hash = Math.imul(hash ^ char.charCodeAt(0), 16777619)
    vector[hash >>> 0 & 255] += 1
  }
  const length = Math.hypot(...vector) || 1
  return vector.map((value) => value / length)
}
const chunks = (content: string) => content.match(/[\s\S]{1,1000}/g) ?? []

const ESA_TEAM = /^[a-z0-9][a-z0-9-]{0,62}$/

/** A mock sync stays queued this long, then runs until MOCK_SYNC_MS after it was picked up. */
export const MOCK_SYNC_QUEUED_MS = 500
export const MOCK_SYNC_MS = 3_000
const MOCK_BACKFILL_DOCUMENTS = 48
const MOCK_BACKFILL_QUEUED_MS = 1_000
const MOCK_BACKFILL_BATCH_MS = 2_000
/** Fixed `expanded` formulations the mock "LLM" adds for any query. */
export const MOCK_EXPANDED = ['年次有給休暇', '休暇申請']

type SyncPlan = { documents: DocumentInput[]; errors: string[]; total: number | null; failure?: string }
type MockJob = { job: IngestJob; source: Source; plan: SyncPlan; startAt: number }
export type MockKnowledgeOptions = {
  llm?: boolean
  platformManaged?: boolean
  esaPlatformManaged?: boolean
  /** GET /providers/esa reports a rejected platform-managed token; implies esa platform management. */
  esaConnectionError?: boolean
  /** `/config` reports a re-embed backfill counting down from its first read. */
  backfill?: boolean
  /** Milliseconds since the epoch; tests drive job and backfill timelines with it. */
  clock?: () => number
}

export function createMockKnowledge({ llm = true, platformManaged = false, esaPlatformManaged: esaManagedOption = false, esaConnectionError = false, backfill = false, clock = Date.now }: MockKnowledgeOptions = {}) {
  const esaPlatformManaged = esaManagedOption || esaConnectionError
  const config = (): Config => ({ projectId: 'pj-knowledge-demo', publicUrl: 'https://knowledge.pj-knowledge-demo.appthrust.test', embedding: { provider: 'hashing', source: 'hashing', dimensions: 256, backfill: backfillStatus() }, llm: llm ? { configured: true, provider: 'gateway' } : { configured: false }, notion: { platformManaged }, esa: { platformManaged: esaPlatformManaged } })
  const timestamp = () => new Date(clock()).toISOString()
  const esaConnection: EsaConnection = !esaPlatformManaged ? { platformManaged: false, connected: false, teams: [] }
    : esaConnectionError ? { platformManaged: true, connected: false, teams: [], error: 'esa returned HTTP 401' }
    : { platformManaged: true, connected: true, teams: [{ name: 'docs', description: 'esa.io official documents', icon: 'https://img.esa.io/mock/docs.png' }, { name: 'engineering', description: 'Engineering handbook and design docs', icon: 'https://img.esa.io/mock/engineering.png' }] }
  const collections = new Map<string, Collection>()
  const documents = new Map<string, KnowledgeDocument>()
  const sources = new Map<string, Source>()
  // Credentials intentionally live outside the public Source DTO.
  const sourceTokens = new Map<string, string>()
  const jobs = new Map<string, MockJob>()
  let workerFreeAt = 0
  const putDocument = (input: DocumentInput) => {
    const existing = input.externalId ? [...documents.values()].find((document) => document.collection === input.collection && document.externalId === input.externalId) : undefined
    const document: KnowledgeDocument = { ...input, id: existing?.id ?? crypto.randomUUID(), chunks: chunks(input.content).length }
    documents.set(document.id, document)
    return document
  }
  collections.set('handbook', { name: 'handbook', description: 'Team handbook · チームハンドブック', visibility: { groups: [] } })
  putDocument({ collection: 'handbook', externalId: 'getting-started', title: 'Welcome to Knowledge', url: 'https://example.com/handbook', content: '# Knowledge\n\nSearch your team handbook with hybrid retrieval. Add Markdown documents or sync a Notion source to start building your knowledge index.' })

  /** What a sync of `source` imports, decided when it is enqueued; `failure` stops the walk itself. */
  const planSync = (source: Source): SyncPlan => {
    const plan: SyncPlan = { documents: [], errors: [], total: null }
    if (source.type === 'esa') {
      const team = source.config.team ?? ''
      const query = source.config.query ?? ''
      if (query.includes('fail:')) return { ...plan, failure: `Mock esa search failed: ${query}` }
      const posts = [{ number: 1, category: 'Engineering/Handbook', name: 'Onboarding', updatedAt: '2026-09-30T10:15:00+09:00', watchers: 9 }, { number: 2, category: 'Engineering/API', name: 'Design guidelines', updatedAt: '2026-10-08T18:40:00+09:00', watchers: 2 }]
      for (const post of posts) plan.documents.push({ collection: source.collection, externalId: `esa:${source.id}:${post.number}`, title: `${post.category}/${post.name}`, url: `https://${team}.esa.io/posts/${post.number}`, content: `# Mock esa post ${post.name}\n\nThis is a local development fixture from ${team}.esa.io, not a real esa import. Knowledge indexes these synced posts for hybrid search.`, metadata: { number: post.number, category: post.category, tags: [], updatedAt: post.updatedAt, watchers: post.watchers, stars: 0, comments: 0 } })
      // esa's post search reports its total; a Notion walk cannot know its size up front.
      plan.total = posts.length
      if (query.includes('error:')) plan.errors.push(`Mock esa post unavailable: ${query}`)
      return plan
    }
    const ids = [...source.config.rootPageIds ?? [], ...source.config.databaseIds ?? []]
    if (ids.some((id) => id.startsWith('fail:'))) return { ...plan, failure: `Mock Notion walk failed: ${ids.find((id) => id.startsWith('fail:'))}` }
    if (!ids.length) plan.errors.push('Mock sync: add a root page ID or database ID.')
    for (const id of ids.slice(0, 100)) {
      if (id.startsWith('error:')) { plan.errors.push(`Mock Notion page unavailable: ${id}`); continue }
      plan.documents.push({ collection: source.collection, externalId: `notion:${source.id}:${id}`, title: `Notion · ${id}`, url: `https://www.notion.so/${encodeURIComponent(id)}`, content: `# Mock Notion page ${id}\n\nThis is a local development fixture, not a real Notion import. Knowledge indexes these synced documents for hybrid search.` })
    }
    if (ids.length > 100) plan.errors.push('Mock sync is bounded to 100 page IDs.')
    return plan
  }
  /** Moves a job along its timeline: queued, then running (documents upserted one by one), then done. */
  const advance = ({ job, source, plan, startAt }: MockJob) => {
    const elapsed = clock() - startAt
    if (job.state === 'queued' && elapsed >= MOCK_SYNC_QUEUED_MS) {
      job.startedAt = timestamp()
      if (plan.failure) {
        Object.assign(job, { state: 'failed', error: plan.failure, finishedAt: timestamp() })
        return
      }
      job.state = 'running'
      job.progress.total = plan.total
    }
    if (job.state !== 'running') return
    const finished = elapsed >= MOCK_SYNC_MS
    const processed = finished ? plan.documents.length : Math.floor(plan.documents.length * (elapsed - MOCK_SYNC_QUEUED_MS) / (MOCK_SYNC_MS - MOCK_SYNC_QUEUED_MS))
    while (job.progress.documents < processed) {
      job.progress.chunks += putDocument(plan.documents[job.progress.documents]).chunks
      job.progress.documents += 1
    }
    if (!finished) return
    // Like the server, an error-free walk prunes this source's documents the walk did not return.
    const externalIds = new Set(plan.documents.map((document) => document.externalId))
    let removed = 0
    if (plan.errors.length === 0) {
      for (const document of documents.values()) {
        if (document.externalId?.startsWith(`${source.type}:${source.id}:`) && !externalIds.has(document.externalId)) { documents.delete(document.id); removed += 1 }
      }
    }
    const result: SyncResult = { documents: job.progress.documents, chunks: job.progress.chunks, removed, errors: plan.errors }
    Object.assign(job, { state: 'done', result, finishedAt: timestamp() })
    source.lastSync = { ...result, finishedAt: job.finishedAt as string }
  }
  /** The re-embed countdown starts at the first `/config` read, so a browser opened later still sees it. */
  let backfillStartedAt: number | undefined
  const backfillStatus = (): EmbeddingBackfill => {
    if (!backfill) return { pending: 0, state: 'idle' }
    backfillStartedAt ??= clock()
    const elapsed = clock() - backfillStartedAt
    if (elapsed < MOCK_BACKFILL_QUEUED_MS) return { pending: MOCK_BACKFILL_DOCUMENTS, state: 'queued' }
    // Re-embedding runs in batches of 8 documents.
    const pending = Math.max(0, MOCK_BACKFILL_DOCUMENTS - 8 * Math.floor((elapsed - MOCK_BACKFILL_QUEUED_MS) / MOCK_BACKFILL_BATCH_MS))
    return { pending, state: pending ? 'running' : 'idle' }
  }

  const search = (input: SearchInput): SearchResult => {
    const mode = input.mode ?? 'hybrid'
    const queryWords = words(input.query)
    const queryVector = embedding(input.query)
    const candidates = [...documents.values()].filter((document) => !input.collections?.length || input.collections.includes(document.collection)).flatMap((document) => chunks(document.content).map((text, chunkIndex) => {
      const vector = embedding(text)
      const lexical = queryWords.reduce((count, word) => count + words(text).filter((token) => token === word).length, 0)
      const cosine = vector.reduce((sum, value, index) => sum + value * queryVector[index], 0)
      // Like the server, metadata is omitted when empty.
      const hit: SearchHit = { documentId: document.id, title: document.title, url: document.url ?? '', collection: document.collection, chunkIndex, text, score: 0, ...(document.metadata && Object.keys(document.metadata).length ? { metadata: document.metadata } : {}) }
      return { hit, lexical, cosine, key: `${document.id}:${chunkIndex}` }
    }))
    const lexical = candidates.filter((entry) => entry.lexical > 0).sort((a, b) => b.lexical - a.lexical)
    const vector = [...candidates].sort((a, b) => b.cosine - a.cosine)
    const ranked = mode === 'lexical' ? lexical : mode === 'vector' ? vector : candidates
    const hits = ranked.map((entry) => {
      const lexRank = lexical.findIndex((candidate) => candidate.key === entry.key)
      const vectorRank = vector.findIndex((candidate) => candidate.key === entry.key)
      const score = mode === 'lexical' ? entry.lexical : mode === 'vector' ? entry.cosine : (lexRank < 0 ? 0 : 1 / (61 + lexRank)) + 1 / (61 + vectorRank)
      return { ...entry.hit, score }
    }).sort((a, b) => b.score - a.score).slice(0, input.topK ?? 8)
    // Deterministic stand-in for the LLM rerank: the top 3 are reversed.
    if (input.rerank) hits.splice(0, 3, ...hits.slice(0, 3).reverse())
    return { hits, mode, reranked: input.rerank === true, expanded: input.expand ? MOCK_EXPANDED : [], embedding: { provider: 'hashing', source: 'hashing' } }
  }

  return async function handle(request: Request): Promise<Response> {
    const url = new URL(request.url)
    const method = request.method
    // Jobs move on wall-clock (or test-clock) time, observed at every request.
    for (const entry of jobs.values()) advance(entry)
    if (method === 'GET' && (url.pathname === '/config' || url.pathname === `${BASE}/config`)) return json(config())
    if (method === 'GET' && url.pathname === '/releasez') return json({ releaseVersion: 'mock', sourceRevision: 'mock-knowledge', artifactDigest: 'mock-only', releaseClosureDigest: 'mock-only' })
    if (method === 'GET' && ['/healthz', '/readyz'].includes(url.pathname)) return new Response('ok')
    if (!url.pathname.startsWith(`${BASE}/`)) return fail(404, 'not_found', 'Unknown mock route')
    const parts = url.pathname.slice(BASE.length + 1).split('/').map(decodeURIComponent)
    try {
      if (parts[0] === 'collections') {
        if (method === 'GET' && parts.length === 1) return json({ data: [...collections.values()] })
        if (method === 'POST' && parts.length === 1) {
          const input = await request.json() as Collection
          if (!input.name?.trim() || !Array.isArray(input.visibility?.groups) || input.visibility.groups.some((group) => typeof group !== 'string')) return fail(400, 'invalid_request', 'Name and visibility groups are required')
          if (collections.has(input.name)) return fail(409, 'collection_exists', 'Collection already exists')
          collections.set(input.name, input)
          return json(input)
        }
        if (method === 'DELETE' && parts.length === 2) {
          if (!collections.delete(parts[1])) return fail(404, 'not_found', 'Collection not found')
          for (const document of documents.values()) if (document.collection === parts[1]) documents.delete(document.id)
          for (const source of sources.values()) if (source.collection === parts[1]) { sources.delete(source.id); sourceTokens.delete(source.id) }
          for (const entry of jobs.values()) if (entry.source.collection === parts[1]) jobs.delete(entry.job.id)
          return new Response(null, { status: 204 })
        }
      }
      if (parts[0] === 'documents') {
        if (method === 'GET' && parts.length === 1) return json({ data: [...documents.values()].filter((document) => !url.searchParams.get('collection') || document.collection === url.searchParams.get('collection')) })
        if (method === 'POST' && parts.length === 1) {
          const input = await request.json() as DocumentInput
          if (!collections.has(input.collection)) return fail(404, 'not_found', 'Collection not found')
          if (!input.title?.trim() || !input.content?.trim() || new TextEncoder().encode(input.content).length > 1024 * 1024) return fail(400, 'invalid_request', 'Title and content (at most 1 MiB) are required')
          const document = putDocument(input)
          return json({ id: document.id, collection: document.collection, chunks: document.chunks })
        }
        const document = documents.get(parts[1])
        if (!document) return fail(404, 'not_found', 'Document not found')
        if (method === 'GET' && parts.length === 2) return json(document)
        if (method === 'DELETE' && parts.length === 2) { documents.delete(document.id); return new Response(null, { status: 204 }) }
      }
      if (method === 'GET' && parts.length === 2 && parts[0] === 'providers' && parts[1] === 'esa') return json(esaConnection)
      if (parts[0] === 'sources') {
        if (method === 'GET' && parts.length === 1) return json({ data: [...sources.values()] })
        if (method === 'POST' && parts.length === 1) {
          const input = await request.json() as SourceInput
          if (input.type === 'esa') {
            if (esaPlatformManaged && input.config?.token !== undefined) return fail(400, 'knowledge_esa_token_platform_managed', "The esa token is managed in the Project's built-in app settings")
            if (!esaPlatformManaged && !input.config?.token?.trim()) return fail(400, 'invalid_request', 'An esa token is required')
            if (!ESA_TEAM.test(input.config.team ?? '')) return fail(400, 'invalid_request', 'An esa team subdomain is required')
          } else {
            if (platformManaged && input.config?.token !== undefined) return fail(400, 'knowledge_notion_token_platform_managed', "The Notion token is managed in the Project's built-in app settings")
            if (input.type !== 'notion' || (!platformManaged && !input.config?.token?.trim())) return fail(400, 'invalid_request', 'A Notion token is required')
          }
          if (!collections.has(input.collection)) return fail(404, 'not_found', 'Collection not found')
          const managed = input.type === 'esa' ? esaPlatformManaged : platformManaged
          const source: Source = input.type === 'esa'
            ? { id: crypto.randomUUID(), type: 'esa', collection: input.collection, tokenSource: managed ? 'platform' : 'stored', config: { team: input.config.team, query: input.config.query ?? '', includeWip: input.config.includeWip === true, includeArchived: input.config.includeArchived === true }, lastSync: null, sync: null }
            : { id: crypto.randomUUID(), type: 'notion', collection: input.collection, tokenSource: managed ? 'platform' : 'stored', config: { rootPageIds: input.config.rootPageIds ?? [], databaseIds: input.config.databaseIds ?? [] }, lastSync: null, sync: null }
          sources.set(source.id, source)
          if (!managed && input.config.token) sourceTokens.set(source.id, input.config.token)
          return json(source)
        }
        const source = sources.get(parts[1])
        if (!source) return fail(404, 'not_found', 'Source not found')
        if (method === 'GET' && parts.length === 2) return json(source)
        if (method === 'DELETE' && parts.length === 2) {
          if (source.sync && (source.sync.state === 'queued' || source.sync.state === 'running')) return fail(409, 'knowledge_source_syncing', 'The source is being synced; wait for the sync to finish')
          let count = 0
          for (const document of documents.values()) {
            if (document.externalId?.startsWith(`${source.type}:${source.id}:`)) { documents.delete(document.id); count += 1 }
          }
          for (const entry of jobs.values()) if (entry.source.id === source.id) jobs.delete(entry.job.id)
          sources.delete(source.id)
          sourceTokens.delete(source.id)
          return json({ deleted: true, documents: count })
        }
        if (method === 'POST' && parts[2] === 'sync' && parts.length === 3) {
          // Idempotent: an in-flight sync of this source is returned as-is.
          if (source.sync && (source.sync.state === 'queued' || source.sync.state === 'running')) return json({ job: source.sync }, 202)
          // One worker, FIFO: a job starts once the previous one has had its full run.
          const startAt = Math.max(clock(), workerFreeAt)
          workerFreeAt = startAt + MOCK_SYNC_MS
          const job: IngestJob = { id: crypto.randomUUID(), kind: 'sync', state: 'queued', progress: { documents: 0, chunks: 0, total: null }, result: null, error: null, createdAt: timestamp(), startedAt: null, finishedAt: null }
          jobs.set(job.id, { job, source, plan: planSync(source), startAt })
          source.sync = job
          return json({ job }, 202)
        }
      }
      if (method === 'GET' && parts.length === 2 && parts[0] === 'jobs') {
        const entry = jobs.get(parts[1])
        return entry ? json(entry.job) : fail(404, 'not_found', 'Job not found')
      }
      if (method === 'POST' && parts.length === 1 && (parts[0] === 'search' || parts[0] === 'answer')) {
        const input = await request.json() as SearchInput
        if (!input.query?.trim() || (input.mode && !(['hybrid', 'lexical', 'vector'] as SearchMode[]).includes(input.mode)) || (input.topK !== undefined && (!Number.isInteger(input.topK) || input.topK < 1 || input.topK > 50)) || (input.rerank !== undefined && typeof input.rerank !== 'boolean') || (input.expand !== undefined && typeof input.expand !== 'boolean')) return fail(400, 'invalid_request', 'Query, search mode, topK, rerank or expand is invalid')
        if (parts[0] === 'search' && (input.rerank || input.expand) && !llm) return fail(503, 'knowledge_llm_unavailable', 'No LLM configured')
        const result = search(parts[0] === 'search' ? input : { ...input, rerank: false, expand: false })
        if (parts[0] === 'search') return json(result)
        if (!llm) return fail(503, 'knowledge_llm_unavailable', 'No LLM configured')
        const encoder = new TextEncoder()
        const events = result.hits.map((hit) => `event: citation\ndata: ${JSON.stringify(hit)}\n\n`)
        const answer = result.hits.length ? `Mock answer (no LLM): ${result.hits[0].text}` : 'Mock answer (no LLM): no matching documents.'
        for (const text of answer.match(/[\s\S]{1,24}/g) ?? []) events.push(`event: token\ndata: ${JSON.stringify({ text })}\n\n`)
        events.push('event: done\ndata: {}\n\n')
        let next = 0
        const stream = new ReadableStream<Uint8Array>({
          async pull(controller) {
            await Bun.sleep(20)
            if (next < events.length) controller.enqueue(encoder.encode(events[next++]))
            else controller.close()
          },
        })
        return new Response(stream, { headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' } })
      }
      return fail(404, 'not_found', 'Unknown mock route')
    } catch (error) {
      return fail(400, 'invalid_request', error instanceof Error ? error.message : 'Invalid JSON body')
    }
  }
}
