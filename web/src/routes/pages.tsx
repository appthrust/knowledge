import { createRootRouteWithContext, createRoute, Outlet } from '@tanstack/react-router'
import type { QueryClient } from '@tanstack/react-query'
import { CollectionsPage } from '../features/collections/collections-page'
import { DocumentsPage } from '../features/documents/documents-page'
import { SearchPage } from '../features/search/search-page'
import { SourcesPage } from '../features/sources/sources-page'
import { Shell } from '../shell/shell'

const root = createRootRouteWithContext<{ queryClient: QueryClient }>()({ component: () => <Shell><Outlet /></Shell> })
export const searchRoute = createRoute({ getParentRoute: () => root, path: '/', component: SearchPage })
export const collectionsRoute = createRoute({ getParentRoute: () => root, path: '/collections', component: CollectionsPage })
export const documentsRoute = createRoute({ getParentRoute: () => root, path: '/documents', component: DocumentsPage })
export const sourcesRoute = createRoute({ getParentRoute: () => root, path: '/sources', component: SourcesPage })
export const routeTree = root.addChildren([searchRoute, collectionsRoute, documentsRoute, sourcesRoute])
