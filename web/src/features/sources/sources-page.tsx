import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { isActiveJob, knowledgeApi } from '../../api/knowledge'
import { collectionsQuery, configQuery, esaConnectionQuery, invalidateKnowledge, sourcesQuery } from '../../api/queries'
import type { EsaConnection, IngestJob, Source, SourceInput, SourceType } from '../../api/types'
import { useText } from '../../i18n'
import { CollectionSelect, ErrorNotice, Field, formText, PageHeading, QueryFeedback, splitIds } from '../../ui'

export function SourcesPage() {
  const { t } = useText()
  const client = useQueryClient()
  const [selected, setSelected] = useState('')
  const [type, setType] = useState<SourceType>('notion')
  const collections = useQuery(collectionsQuery)
  // Syncs run as background jobs: poll while any is queued or running.
  const sources = useQuery({ ...sourcesQuery, refetchInterval: (query) => query.state.data?.some((source) => isActiveJob(source.sync)) ? 2_000 : false })
  const config = useQuery(configQuery)
  // Fetched only once esa is chosen: the probe calls esa's API with the Project's token.
  const esaConnection = useQuery({ ...esaConnectionQuery, enabled: type === 'esa' })
  const esaTeams = esaConnection.data?.platformManaged && esaConnection.data.connected ? esaConnection.data.teams : []
  const notionManaged = config.data?.notion.platformManaged === true
  const esaManaged = config.data?.esa.platformManaged === true
  const settingsUrl = config.data ? installedAppSettingsUrl(config.data.publicUrl, config.data.projectId) : undefined
  const collection = selected || collections.data?.[0]?.name || ''
  const create = useMutation({ mutationFn: knowledgeApi.sources.create, onSuccess: () => invalidateKnowledge(client) })
  const sync = useMutation({
    mutationFn: knowledgeApi.sources.sync,
    onSuccess: (job, id) => {
      // Show the queued job (and start polling) before the list refetch lands.
      client.setQueryData(sourcesQuery.queryKey, (current) => current?.map((source) => source.id === id ? { ...source, sync: job } : source))
      void invalidateKnowledge(client)
    },
  })
  const remove = useMutation({ mutationFn: knowledgeApi.sources.remove, onSuccess: () => invalidateKnowledge(client) })
  // A finished sync changed documents and may have queued a re-embed: refresh everything once.
  const syncActive = sources.data?.some((source) => isActiveJob(source.sync)) ?? false
  const wasSyncActive = useRef(syncActive)
  useEffect(() => {
    if (wasSyncActive.current && !syncActive) void invalidateKnowledge(client)
    wasSyncActive.current = syncActive
  }, [client, syncActive])
  return <>
    <PageHeading title={t('knowledge.sources')}>{t('knowledge.sourcesHelp')}</PageHeading>
    <div className="workspace-grid">
      <section aria-label={t('knowledge.sources')}>
        <QueryFeedback pending={sources.isPending} error={sources.error} retry={sources.refetch} />
        <ErrorNotice error={sync.error} />
        <ErrorNotice error={remove.error} />
        {sources.data && <SourcesList sources={sources.data} requesting={sync.isPending ? sync.variables : undefined} busy={remove.isPending} onSync={(id) => sync.mutate(id)} onRemove={(source) => { if (window.confirm(t('knowledge.deleteSource', { name: sourceName(source) }))) remove.mutate(source.id) }} />}
      </section>
      <form className="editor" onSubmit={(event) => {
        event.preventDefault()
        const form = event.currentTarget
        const data = new FormData(form)
        const query = formText(data, 'query')
        const input: SourceInput = type === 'esa'
          ? { type, collection, config: { ...(esaManaged ? {} : { token: formText(data, 'esaToken') }), team: formText(data, 'team'), ...(query ? { query } : {}), includeWip: data.get('includeWip') === 'on', includeArchived: data.get('includeArchived') === 'on' } }
          : { type, collection, config: { ...(notionManaged ? {} : { token: formText(data, 'token') }), rootPageIds: splitIds(formText(data, 'pages')), databaseIds: splitIds(formText(data, 'databases')) } }
        create.mutate(input, { onSuccess: () => form.reset() })
        // Keep credentials only for the in-flight write, never in visible state.
        for (const name of ['token', 'esaToken']) {
          const token = form.elements.namedItem(name)
          if (token instanceof HTMLInputElement) token.value = ''
        }
      }}>
        <h2>{t('knowledge.addSource')}</h2>
        <QueryFeedback pending={collections.isPending} error={collections.error} retry={collections.refetch} />
        <CollectionSelect collections={collections.data ?? []} value={collection} onChange={setSelected} />
        <Field label={t('knowledge.sourceType')}><select name="type" value={type} onChange={(event) => setType(event.target.value as SourceType)}><option value="notion">{t('knowledge.sourceNotion')}</option><option value="esa">{t('knowledge.sourceEsa')}</option></select></Field>
        <QueryFeedback pending={config.isPending} error={config.error} retry={config.refetch} />
        {/* Both branches stay mounted so typed values survive switching; the inactive one is hidden and disabled, so it is neither validated nor submitted. */}
        <fieldset hidden={type !== 'notion'} disabled={type !== 'notion'}>
          {config.data && <TokenField name="token" managed={notionManaged} settingsUrl={settingsUrl} label={t('knowledge.token')} hint={t('knowledge.tokenHelp')} managedText={t('knowledge.tokenPlatformManaged')} />}
          <Field label={t('knowledge.rootPages')} hint={t('knowledge.idsHelp')}><textarea name="pages" rows={2} /></Field>
          <Field label={t('knowledge.databases')} hint={t('knowledge.idsHelp')}><textarea name="databases" rows={2} /></Field>
        </fieldset>
        <fieldset hidden={type !== 'esa'} disabled={type !== 'esa'}>
          <QueryFeedback pending={esaConnection.isLoading} error={esaConnection.error} retry={esaConnection.refetch} />
          {esaConnection.data && <EsaConnectionNotice connection={esaConnection.data} settingsUrl={settingsUrl} />}
          {config.data && <TokenField name="esaToken" managed={esaManaged} settingsUrl={settingsUrl} label={t('knowledge.esaToken')} hint={t('knowledge.esaTokenHelp')} managedText={t('knowledge.esaTokenPlatformManaged')} />}
          {esaTeams.length > 0
            ? <Field label={t('knowledge.esaTeam')} hint={t('knowledge.esaTeamSelectHelp')}><select key={esaTeams.map((team) => team.name).join()} name="team" required defaultValue={esaTeams[0].name}>{esaTeams.map((team) => <option key={team.name} value={team.name}>{team.name}</option>)}</select></Field>
            : <Field label={t('knowledge.esaTeam')} hint={t('knowledge.esaTeamHelp')}><input name="team" required pattern="[a-z0-9][a-z0-9\-]*" maxLength={63} autoComplete="off" autoCapitalize="none" spellCheck={false} /></Field>}
          <Field label={t('knowledge.esaQuery')} hint={t('knowledge.esaQueryHelp')}><input name="query" autoComplete="off" spellCheck={false} /></Field>
          <label className="field"><span className="check"><input name="includeWip" type="checkbox" />{t('knowledge.esaIncludeWip')}</span><small>{t('knowledge.esaIncludeWipHelp')}</small></label>
          <label className="field"><span className="check"><input name="includeArchived" type="checkbox" />{t('knowledge.esaIncludeArchived')}</span><small>{t('knowledge.esaIncludeArchivedHelp')}</small></label>
        </fieldset>
        <ErrorNotice error={create.error} />
        <button disabled={create.isPending || !collection || !config.data}>{t(create.isPending ? 'knowledge.saving' : 'knowledge.addSource')}</button>
      </form>
    </div>
  </>
}
function TokenField({ name, managed, settingsUrl, label, hint, managedText }: { name: string; managed: boolean; settingsUrl?: string; label: string; hint: string; managedText: string }) {
  if (managed) return <p className="field">{settingsUrl ? <a href={settingsUrl}>{managedText}</a> : managedText}</p>
  return <Field label={label} hint={hint}><input name={name} type="password" required autoComplete="new-password" /></Field>
}
function EsaConnectionNotice({ connection, settingsUrl }: { connection: EsaConnection; settingsUrl?: string }) {
  const { t } = useText()
  const settingsLink = (text: string) => <p>{settingsUrl ? <a href={settingsUrl}>{text}</a> : text}</p>
  if (!connection.platformManaged) return <div className="notice info"><strong>{t('knowledge.esaNotConnected')}</strong>{settingsLink(t('knowledge.esaConnectInSettings'))}</div>
  if (!connection.connected) return <div className="notice error" role="alert"><strong>{t('knowledge.esaConnectionFailed')}</strong>{connection.error && <p>{connection.error}</p>}{settingsLink(t('knowledge.esaReplaceToken'))}</div>
  return <div className="notice success" role="status">
    <strong>{t('knowledge.esaConnected')}</strong>
    {connection.teams.length ? <ul className="team-list" aria-label={t('knowledge.esaTeams')}>{connection.teams.map((team) => <li key={team.name}>
      {/* Team icons come from esa; only https images are rendered. */}
      {team.icon && /^https:\/\//i.test(team.icon) && <img src={team.icon} alt="" loading="lazy" referrerPolicy="no-referrer" onError={(event) => { event.currentTarget.hidden = true }} />}
      <span><span className="mono">{team.name}</span>{team.description && <small> · {team.description}</small>}</span>
    </li>)}</ul> : <p>{t('knowledge.esaNoTeams')}</p>}
  </div>
}
function sourceName(source: Source): string {
  return source.type === 'esa' ? `esa · ${source.config.team ?? ''}` : `Notion · ${source.collection}`
}
/** `requesting`: the source whose Sync now request is in flight; `busy`: a delete is in flight. */
export function SourcesList({ sources, requesting, busy, onSync, onRemove }: { sources: Source[]; requesting?: string; busy?: boolean; onSync: (id: string) => void; onRemove: (source: Source) => void }) {
  const { t, locale } = useText()
  if (sources.length === 0) return <p className="empty">{t('knowledge.emptySources')}</p>
  return <ul className="resource-list">{sources.map((source) => {
    const syncing = requesting === source.id || isActiveJob(source.sync)
    return <li key={source.id}>
      {/* The server refuses deletion while a sync is active (409), so the button follows the same state. */}
      <div className="row"><h2>{sourceName(source)}</h2><span className="actions"><button type="button" className="secondary" disabled={syncing} onClick={() => onSync(source.id)}>{t(syncing ? 'knowledge.syncing' : 'knowledge.sync')}</button><button type="button" className="danger" disabled={syncing || busy} onClick={() => onRemove(source)}>{t('knowledge.delete')}</button></span></div>
      <p className="mono muted">{source.id}</p>
      {source.type === 'esa' ? <>
        <p>{t('knowledge.collection')}: {source.collection}</p>
        <p>{t('knowledge.esaQuery')}: {source.config.query || '—'}</p>
        <p>{t(source.config.includeWip ? 'knowledge.esaWipIncluded' : 'knowledge.esaWipExcluded')} · {t(source.config.includeArchived ? 'knowledge.esaArchivedIncluded' : 'knowledge.esaArchivedExcluded')}</p>
      </> : <>
        <p>{t('knowledge.rootPages')}: {source.config.rootPageIds?.join(', ') || '—'}</p>
        <p>{t('knowledge.databases')}: {source.config.databaseIds?.join(', ') || '—'}</p>
      </>}
      {/* Always mounted, so a job moving to running, done or failed is announced. */}
      <div className="sync-status" aria-live="polite">{source.sync && <SyncJobStatus job={source.sync} />}</div>
      <h3>{t('knowledge.lastSync')}</h3>
      {source.lastSync ? <>
        <p>{t('knowledge.syncResult', { documents: source.lastSync.documents, chunks: source.lastSync.chunks })}{source.lastSync.removed > 0 && ` · ${t('knowledge.syncRemoved', { removed: source.lastSync.removed })}`}</p>
        <time dateTime={source.lastSync.finishedAt}>{new Date(source.lastSync.finishedAt).toLocaleString(locale)}</time>
        {source.lastSync.errors.length ? <div className="notice error"><h3>{t('knowledge.syncErrors')}</h3><ul>{source.lastSync.errors.map((error, index) => <li key={index}>{error}</li>)}</ul></div> : <p className="muted">{t('knowledge.noSyncErrors')}</p>}
      </> : <p className="muted">{t('knowledge.notSynced')}</p>}
    </li>
  })}</ul>
}
/** The latest job while it is queued, running or failed; a done job is shown as the last sync. */
function SyncJobStatus({ job }: { job: IngestJob }) {
  const { t } = useText()
  const { documents, chunks, total } = job.progress
  if (job.state === 'failed') return <div className="notice error"><strong>{t('knowledge.syncFailed')}</strong>{job.error && <p>{job.error}</p>}<p>{t('knowledge.syncFailedHelp')}</p></div>
  // Queued is waiting, not working: no bar until the worker picks it up.
  if (job.state === 'queued') return <div className="sync-job"><p>{t('knowledge.syncQueued')}</p></div>
  if (job.state !== 'running') return null
  // Without a known total the bar stays indeterminate.
  return <div className="sync-job">
    <p>{total === null ? t('knowledge.syncRunning', { documents, chunks }) : t('knowledge.syncRunningTotal', { documents, chunks, total })}</p>
    {total === null ? <progress aria-label={t('knowledge.syncProgress')} /> : <progress aria-label={t('knowledge.syncProgress')} max={Math.max(total, 1)} value={documents} />}
  </div>
}

export function installedAppSettingsUrl(publicUrl: string, projectId: string): string | undefined {
  try {
    const url = new URL(publicUrl)
    const labels = url.hostname.split('.')
    if (url.protocol !== 'https:' || labels[1] !== projectId || labels.length < 3) return undefined
    url.hostname = ['dashboard', ...labels.slice(2)].join('.')
    url.pathname = `/dashboard/projects/${encodeURIComponent(projectId)}/marketplace/knowledge/credentials`
    url.search = ''
    url.hash = ''
    return url.href
  } catch { return undefined }
}
