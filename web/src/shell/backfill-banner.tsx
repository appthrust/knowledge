import { useQuery } from '@tanstack/react-query'
import { isActiveBackfill } from '../api/knowledge'
import { configQuery } from '../api/queries'
import { useText } from '../i18n'

/**
 * Shown on every page while the server re-embeds documents written with a
 * previous embedding model; `/config` is polled only while that runs.
 */
export function BackfillBanner() {
  const { t } = useText()
  const config = useQuery({ ...configQuery, refetchInterval: (query) => isActiveBackfill(query.state.data?.embedding.backfill) ? 5_000 : false })
  const backfill = config.data?.embedding.backfill
  // Always mounted, so the countdown and its end are announced politely.
  return <div className="backfill-banner" role="status">
    {isActiveBackfill(backfill) && <p><span className="pulse" aria-hidden />{t('knowledge.backfill', { count: backfill?.pending ?? 0 })}</p>}
  </div>
}
