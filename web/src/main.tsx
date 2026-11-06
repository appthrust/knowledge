import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { queryClient } from './api/queries'
import { I18nProvider, useLanguage } from './i18n'
import { router } from './router'
import './styles.css'

// A stale deployment chunk gets one full navigation per tab session, never a reload loop.
window.addEventListener('vite:preloadError', (event) => {
  const key = 'knowledge:preload-reloaded'
  try {
    if (sessionStorage.getItem(key)) return
    sessionStorage.setItem(key, '1')
  } catch {
    // Without persistent storage a reload cannot be bounded across document loads.
    return
  }
  event.preventDefault()
  window.location.reload()
})

function LocalizedRouter() {
  const language = useLanguage()
  // TanStack Router memoizes route matches; remount on a language change so
  // every page reads fresh copy. The router (URL) and query cache survive.
  return (
    <I18nProvider language={language}>
      <RouterProvider key={language} router={router} />
    </I18nProvider>
  )
}

const root = document.getElementById('root')
if (!root) throw new Error('#root is missing from index.html')

createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <LocalizedRouter />
    </QueryClientProvider>
  </StrictMode>
)
