import type { AnswerEvent, AnswerInput, Collection, Config, DocumentInput, DocumentReceipt, EmbeddingBackfill, EsaConnection, IngestJob, KnowledgeDocument, ReleaseIdentity, SearchHit, SearchInput, SearchResult, Source, SourceInput } from './types'

const BASE = '/api/knowledge/v1'
export type Fetcher = (input: string, init: RequestInit) => Promise<Response>

export class KnowledgeApiError extends Error {
  constructor(readonly status: number, readonly code: string, message: string) {
    super(message)
    this.name = 'KnowledgeApiError'
  }
}

async function checked(response: Response) {
  if (response.ok) return response
  let body: { error?: string; message?: string } = {}
  try { body = await response.json() } catch { /* A proxy may return non-JSON. */ }
  throw new KnowledgeApiError(response.status, body.error ?? 'unknown', body.message ?? body.error ?? `knowledge api ${response.status}`)
}

function record(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid knowledge API response')
  return value as Record<string, unknown>
}

export function parseHit(value: unknown): SearchHit {
  const hit = record(value)
  for (const key of ['documentId', 'title', 'url', 'collection', 'text']) {
    if (typeof hit[key] !== 'string') throw new Error(`Invalid search hit: ${key}`)
  }
  if (!Number.isInteger(hit.chunkIndex) || Number(hit.chunkIndex) < 0) throw new Error('Invalid search hit: chunkIndex')
  if (typeof hit.score !== 'number' || !Number.isFinite(hit.score)) throw new Error('Invalid search hit: score')
  if (hit.metadata !== undefined && (!hit.metadata || typeof hit.metadata !== 'object' || Array.isArray(hit.metadata))) throw new Error('Invalid search hit: metadata')
  return hit as SearchHit
}

export function parseSearchResult(value: unknown): SearchResult {
  const result = record(value)
  if (!Array.isArray(result.hits)) throw new Error('Invalid search response: hits')
  if (!['hybrid', 'lexical', 'vector'].includes(String(result.mode))) throw new Error('Invalid search response: mode')
  const embedding = record(result.embedding)
  if (typeof embedding.provider !== 'string') throw new Error('Invalid search response: embedding')
  if (result.reranked !== undefined && typeof result.reranked !== 'boolean') throw new Error('Invalid search response: reranked')
  // `expanded` is empty (or null) unless expand was requested.
  const expanded = result.expanded ?? []
  if (!Array.isArray(expanded) || expanded.some((value) => typeof value !== 'string')) throw new Error('Invalid search response: expanded')
  return { hits: result.hits.map(parseHit), mode: result.mode as SearchResult['mode'], reranked: result.reranked === true, expanded, embedding: { provider: embedding.provider, source: typeof embedding.source === 'string' ? embedding.source : embedding.provider } }
}

const count = (value: unknown) => Number.isInteger(value) && Number(value) >= 0
/** A sync job is still in flight; the Sources list polls while any is. */
export const isActiveJob = (job: IngestJob | null | undefined): job is IngestJob => job?.state === 'queued' || job?.state === 'running'
/** The re-embed backfill is in flight; the banner shows and `/config` is polled while it is. */
export const isActiveBackfill = (backfill: EmbeddingBackfill | undefined) => backfill?.state === 'queued' || backfill?.state === 'running'

export function parseJob(value: unknown): IngestJob {
  const job = record(value)
  if (typeof job.id !== 'string' || !job.id) throw new Error('Invalid job: id')
  if (job.kind !== 'sync' && job.kind !== 'reembed') throw new Error('Invalid job: kind')
  if (!['queued', 'running', 'done', 'failed'].includes(String(job.state))) throw new Error('Invalid job: state')
  const progress = record(job.progress)
  // A re-embed job reports no chunk count.
  if (!count(progress.documents) || (progress.chunks !== undefined && !count(progress.chunks)) || (progress.total != null && !count(progress.total))) throw new Error('Invalid job: progress')
  if (job.error != null && typeof job.error !== 'string') throw new Error('Invalid job: error')
  return { ...job, progress: { documents: Number(progress.documents), chunks: Number(progress.chunks ?? 0), total: progress.total == null ? null : Number(progress.total) }, result: (job.result ?? null) as IngestJob['result'], error: (job.error ?? null) as string | null } as IngestJob
}

export function parseConfig(value: unknown): Config {
  const config = record(value)
  const embedding = record(config.embedding)
  const llm = record(config.llm)
  const backfill = record(embedding.backfill)
  if (!count(backfill.pending) || !['idle', 'queued', 'running', 'failed'].includes(String(backfill.state))) throw new Error('Invalid config: embedding.backfill')
  return {
    ...config,
    embedding: { ...embedding, source: typeof embedding.source === 'string' ? embedding.source : embedding.provider },
    llm: { ...llm, provider: typeof llm.provider === 'string' ? llm.provider : undefined },
  } as Config
}

/** Incremental SSE parsing: UTF-8, CRLF and event boundaries may span reads. */
export async function consumeAnswer(response: Response, onEvent: (event: AnswerEvent) => void): Promise<void> {
  await checked(response)
  if (!response.headers.get('content-type')?.startsWith('text/event-stream') || !response.body) throw new Error('Invalid answer response: expected event stream')
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let event = ''
  let data: string[] = []
  let complete = false
  const line = (text: string) => {
    if (text === '') {
      if (data.length === 0) { event = ''; return }
      const value: unknown = JSON.parse(data.join('\n'))
      if (event === 'citation') onEvent({ type: 'citation', data: parseHit(value) })
      if (event === 'token') {
        const token = record(value)
        if (typeof token.text !== 'string') throw new Error('Invalid answer token: text')
        onEvent({ type: 'token', data: { text: token.text } })
      }
      if (event === 'error') {
        const error = record(value)
        throw new Error(typeof error.message === 'string' ? error.message : String(error.error ?? 'knowledge_answer_failed'))
      }
      if (event === 'done') { complete = true; onEvent({ type: 'done' }) }
      event = ''
      data = []
      return
    }
    if (text.startsWith(':')) return
    const colon = text.indexOf(':')
    const field = colon < 0 ? text : text.slice(0, colon)
    const value = colon < 0 ? '' : text.slice(colon + 1).replace(/^ /, '')
    if (field === 'event') event = value
    if (field === 'data') data.push(value)
  }
  try {
    while (!complete) {
      const next = await reader.read()
      buffer += decoder.decode(next.value, { stream: !next.done })
      let newline: number
      while ((newline = buffer.indexOf('\n')) >= 0 && !complete) {
        line(buffer.slice(0, newline).replace(/\r$/, ''))
        buffer = buffer.slice(newline + 1)
      }
      if (next.done) break
    }
    if (!complete) throw new Error('Answer stream ended before done')
  } finally {
    await reader.cancel()
    reader.releaseLock()
  }
}

export function createKnowledgeApi(fetcher: Fetcher = (input, init) => fetch(input, init)): KnowledgeApi {
  const send = (path: string, method: string, body?: unknown, signal?: AbortSignal) => fetcher(path, {
    method,
    credentials: 'same-origin',
    headers: { Accept: path.endsWith('/answer') ? 'text/event-stream' : 'application/json', ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal,
  })
  async function call<T>(path: string, method = 'GET', body?: unknown, signal?: AbortSignal): Promise<T> {
    const response = await checked(await send(path, method, body, signal))
    return response.status === 204 ? undefined as T : await response.json() as T
  }
  const list = async <T>(path: string, signal?: AbortSignal) => (await call<{ data: T[] }>(`${BASE}${path}`, 'GET', undefined, signal)).data
  return {
    config: async (signal?: AbortSignal) => parseConfig(await call<unknown>('/config', 'GET', undefined, signal)),
    release: (signal?: AbortSignal) => call<ReleaseIdentity>('/releasez', 'GET', undefined, signal),
    collections: {
      list: (signal?: AbortSignal) => list<Collection>('/collections', signal),
      create: (input: Collection) => call<Collection>(`${BASE}/collections`, 'POST', input),
      remove: (name: string) => call<void>(`${BASE}/collections/${encodeURIComponent(name)}`, 'DELETE'),
    },
    documents: {
      list: (collection: string, signal?: AbortSignal) => list<KnowledgeDocument>(`/documents?${new URLSearchParams({ collection })}`, signal),
      get: (id: string) => call<KnowledgeDocument>(`${BASE}/documents/${encodeURIComponent(id)}`),
      create: (input: DocumentInput) => call<DocumentReceipt>(`${BASE}/documents`, 'POST', input),
      remove: (id: string) => call<void>(`${BASE}/documents/${encodeURIComponent(id)}`, 'DELETE'),
    },
    sources: {
      // Polling and progress read `sync`, so it is validated like a job response.
      list: async (signal?: AbortSignal) => (await list<Source>('/sources', signal)).map((source) => source.sync ? { ...source, sync: parseJob(source.sync) } : source),
      get: async (id: string) => {
        const source = await call<Source>(`${BASE}/sources/${encodeURIComponent(id)}`)
        return source.sync ? { ...source, sync: parseJob(source.sync) } : source
      },
      create: (input: SourceInput) => call<Source>(`${BASE}/sources`, 'POST', input),
      sync: async (id: string) => parseJob((await call<{ job: unknown }>(`${BASE}/sources/${encodeURIComponent(id)}/sync`, 'POST')).job),
      remove: (id: string) => call<{ deleted: boolean; documents: number }>(`${BASE}/sources/${encodeURIComponent(id)}`, 'DELETE'),
    },
    jobs: {
      get: async (id: string, signal?: AbortSignal) => parseJob(await call<unknown>(`${BASE}/jobs/${encodeURIComponent(id)}`, 'GET', undefined, signal)),
    },
    providers: {
      esa: (signal?: AbortSignal) => call<EsaConnection>(`${BASE}/providers/esa`, 'GET', undefined, signal),
    },
    search: async (input: SearchInput) => parseSearchResult(await call<unknown>(`${BASE}/search`, 'POST', input)),
    answer: async (input: AnswerInput, onEvent: (event: AnswerEvent) => void, signal?: AbortSignal) => consumeAnswer(await send(`${BASE}/answer`, 'POST', input, signal), onEvent),
  }
}
export interface KnowledgeApi {
  config(signal?: AbortSignal): Promise<Config>
  release(signal?: AbortSignal): Promise<ReleaseIdentity>
  collections: {
    list(signal?: AbortSignal): Promise<Collection[]>
    create(input: Collection): Promise<Collection>
    remove(name: string): Promise<void>
  }
  documents: {
    list(collection: string, signal?: AbortSignal): Promise<KnowledgeDocument[]>
    get(id: string): Promise<KnowledgeDocument>
    create(input: DocumentInput): Promise<DocumentReceipt>
    remove(id: string): Promise<void>
  }
  sources: {
    list(signal?: AbortSignal): Promise<Source[]>
    get(id: string): Promise<Source>
    create(input: SourceInput): Promise<Source>
    /** Enqueues a sync (202); an in-flight sync job of the source is returned as-is. */
    sync(id: string): Promise<IngestJob>
    /** Deletes the source and every document it ingested; 409 knowledge_source_syncing while its sync job is active. */
    remove(id: string): Promise<{ deleted: boolean; documents: number }>
  }
  jobs: {
    get(id: string, signal?: AbortSignal): Promise<IngestJob>
  }
  providers: {
    esa(signal?: AbortSignal): Promise<EsaConnection>
  }
  search(input: SearchInput): Promise<SearchResult>
  answer(input: AnswerInput, onEvent: (event: AnswerEvent) => void, signal?: AbortSignal): Promise<void>
}
export const knowledgeApi = createKnowledgeApi()
