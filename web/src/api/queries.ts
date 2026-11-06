import { QueryClient, queryOptions } from '@tanstack/react-query'
import { knowledgeApi } from './knowledge'

export const KNOWLEDGE_KEY = ['knowledge'] as const
export const queryClient = new QueryClient({ defaultOptions: { queries: { staleTime: 10_000, retry: false } } })
export const configQuery = queryOptions({ queryKey: [...KNOWLEDGE_KEY, 'config'], queryFn: ({ signal }) => knowledgeApi.config(signal) })
export const releaseQuery = queryOptions({ queryKey: [...KNOWLEDGE_KEY, 'release'], queryFn: ({ signal }) => knowledgeApi.release(signal) })
export const collectionsQuery = queryOptions({ queryKey: [...KNOWLEDGE_KEY, 'collections'], queryFn: ({ signal }) => knowledgeApi.collections.list(signal) })
export const documentsQuery = (collection: string) => queryOptions({ queryKey: [...KNOWLEDGE_KEY, 'documents', collection], queryFn: ({ signal }) => knowledgeApi.documents.list(collection, signal), enabled: Boolean(collection) })
export const sourcesQuery = queryOptions({ queryKey: [...KNOWLEDGE_KEY, 'sources'], queryFn: ({ signal }) => knowledgeApi.sources.list(signal) })
export const esaConnectionQuery = queryOptions({ queryKey: [...KNOWLEDGE_KEY, 'providers', 'esa'], queryFn: ({ signal }) => knowledgeApi.providers.esa(signal), staleTime: 60_000 })
export const invalidateKnowledge = (client: QueryClient) => client.invalidateQueries({ queryKey: KNOWLEDGE_KEY })
