import { createMockLauncherHandler, mockLauncherEntries } from '@appthrust/app-launcher'
import { createMockKnowledge } from './mock-knowledge'

const knowledge = createMockKnowledge({ llm: process.env.KNOWLEDGE_MOCK_LLM !== '0', platformManaged: process.env.KNOWLEDGE_MOCK_NOTION_PLATFORM_MANAGED === '1', esaPlatformManaged: process.env.KNOWLEDGE_MOCK_ESA_PLATFORM_MANAGED === '1', esaConnectionError: process.env.KNOWLEDGE_MOCK_ESA_CONNECTION_ERROR === '1', backfill: process.env.KNOWLEDGE_MOCK_BACKFILL === '1' })
const launcher = createMockLauncherHandler(mockLauncherEntries('knowledge'))

const server = Bun.serve({
  hostname: '127.0.0.1',
  port: Number(process.env.KNOWLEDGE_MOCK_PORT ?? 5206),
  fetch: (request) => launcher(request) ?? knowledge(request),
})
console.log(`knowledge mock API on http://${server.hostname}:${server.port}`)
