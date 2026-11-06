import { createRouter } from '@tanstack/react-router'
import { queryClient } from './api/queries'
import { useText } from './i18n'
import { routeTree } from './routes/pages'
import { ErrorNotice } from './ui'

export const router = createRouter({
  routeTree,
  context: { queryClient },
  scrollRestoration: true,
  defaultErrorComponent: ({ error }) => <ErrorNotice error={error} />,
  defaultNotFoundComponent: NotFound,
})
function NotFound() {
  const { t } = useText()
  return <h1>{t('knowledge.notFound')}</h1>
}
declare module '@tanstack/react-router' {
  interface Register { router: typeof router }
}
