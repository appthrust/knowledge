/** Re-embedding of chunks written by another embedding provider; `pending` counts documents. */
export type EmbeddingBackfill = { pending: number; state: 'idle' | 'queued' | 'running' | 'failed' }
export type Config = {
  projectId: string
  publicUrl: string
  embedding: { provider: string; source: string; dimensions: number; backfill: EmbeddingBackfill }
  llm: { configured: boolean; provider?: string }
  notion: { platformManaged: boolean }
  esa: { platformManaged: boolean }
}
export type ReleaseIdentity = { releaseVersion?: string; sourceRevision?: string; artifactDigest?: string; releaseClosureDigest?: string }
export type Collection = { name: string; description?: string; visibility: { groups: string[] } }
export type DocumentInput = { collection: string; externalId?: string; title: string; url?: string; content: string; metadata?: Record<string, unknown> }
export type KnowledgeDocument = DocumentInput & { id: string; chunks: number }
export type DocumentReceipt = { id: string; collection: string; chunks: number }
export type SourceType = 'notion' | 'esa'
export type NotionSourceConfig = { token?: string; rootPageIds?: string[]; databaseIds?: string[] }
/** `includeArchived` false (default) makes the sync exclude esa's top-level Archived category. */
export type EsaSourceConfig = { token?: string; team: string; query?: string; includeWip?: boolean; includeArchived?: boolean }
export type SourceInput = { type: 'notion'; collection: string; config: NotionSourceConfig } | { type: 'esa'; collection: string; config: EsaSourceConfig }
/** `removed`: documents deleted because an error-free walk no longer returned them (0 when the walk had errors). */
export type SyncResult = { documents: number; chunks: number; removed: number; errors: string[] }
export type JobState = 'queued' | 'running' | 'done' | 'failed'
/**
 * A background ingest job. `progress.total` is null while the walk cannot know
 * the size (Notion); `result` is set once done; `error` only when the walk itself failed.
 */
export type IngestJob = {
  id: string
  kind: 'sync' | 'reembed'
  state: JobState
  progress: { documents: number; chunks: number; total: number | null }
  result: SyncResult | null
  error: string | null
  createdAt: string
  startedAt: string | null
  finishedAt: string | null
}
/** `sync` is the latest job of the source; `lastSync` the result of the last finished one. */
export type Source = { id: string; type: SourceType; collection: string; tokenSource?: 'platform' | 'stored'; config: { rootPageIds?: string[]; databaseIds?: string[]; team?: string; query?: string; includeWip?: boolean; includeArchived?: boolean }; lastSync?: (SyncResult & { finishedAt: string }) | null; sync?: IngestJob | null }
export type EsaTeam = { name: string; description?: string; icon?: string }
/** GET /providers/esa: whether the platform-managed esa token is bound and which teams it can read. */
export type EsaConnection = { platformManaged: boolean; connected: boolean; teams: EsaTeam[]; error?: string }
export type SearchMode = 'hybrid' | 'lexical' | 'vector'
/**
 * `rerank` re-orders the top results and `expand` adds LLM-generated official terms and rephrasings;
 * without a usable LLM the server answers knowledge_llm_unavailable (503/502).
 */
export type SearchInput = { query: string; collections?: string[]; topK?: number; mode?: SearchMode; rerank?: boolean; expand?: boolean }
/** `metadata` is connector metadata (esa: number, category, tags, updatedAt, watchers, stars, comments). */
export type SearchHit = { documentId: string; title: string; url: string; collection: string; chunkIndex: number; text: string; score: number; metadata?: Record<string, unknown> }
/** `expanded`: the formulations the LLM added when `expand` was requested; empty otherwise. */
export type SearchResult = { hits: SearchHit[]; mode: SearchMode; reranked: boolean; expanded: string[]; embedding: { provider: string; source: string } }
export type AnswerInput = Pick<SearchInput, 'query' | 'collections'>
export type AnswerEvent = { type: 'citation'; data: SearchHit } | { type: 'token'; data: { text: string } } | { type: 'done' }
