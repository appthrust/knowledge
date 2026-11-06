import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { knowledgeApi } from '../../api/knowledge'
import { collectionsQuery, invalidateKnowledge } from '../../api/queries'
import type { Collection } from '../../api/types'
import { useText } from '../../i18n'
import { ErrorNotice, Field, formText, PageHeading, QueryFeedback, splitIds } from '../../ui'

export function CollectionsPage() {
  const { t } = useText()
  const client = useQueryClient()
  const collections = useQuery(collectionsQuery)
  const create = useMutation({ mutationFn: knowledgeApi.collections.create, onSuccess: () => invalidateKnowledge(client) })
  const remove = useMutation({ mutationFn: knowledgeApi.collections.remove, onSuccess: () => invalidateKnowledge(client) })
  return <>
    <PageHeading title={t('knowledge.collections')}>{t('knowledge.collectionsHelp')}</PageHeading>
    <div className="workspace-grid">
      <section aria-label={t('knowledge.collections')}>
        <QueryFeedback pending={collections.isPending} error={collections.error} retry={collections.refetch} />
        <ErrorNotice error={remove.error} />
        {collections.data && <CollectionsList collections={collections.data} busy={remove.isPending} onRemove={(name) => { if (window.confirm(t('knowledge.deleteCollection', { name }))) remove.mutate(name) }} />}
      </section>
      <form className="editor" onSubmit={(event) => {
        event.preventDefault()
        const form = event.currentTarget
        const data = new FormData(form)
        create.mutate({ name: formText(data, 'name'), description: formText(data, 'description'), visibility: { groups: splitIds(formText(data, 'groups')) } }, { onSuccess: () => form.reset() })
      }}>
        <h2>{t('knowledge.createCollection')}</h2>
        <Field label={t('knowledge.name')}><input name="name" required maxLength={128} /></Field>
        <Field label={t('knowledge.description')}><textarea name="description" rows={2} /></Field>
        <Field label={t('knowledge.groups')} hint={t('knowledge.groupsHelp')}><textarea name="groups" rows={2} /></Field>
        <ErrorNotice error={create.error} />
        <button disabled={create.isPending}>{t(create.isPending ? 'knowledge.saving' : 'knowledge.createCollection')}</button>
      </form>
    </div>
  </>
}
export function CollectionsList({ collections, busy, onRemove }: { collections: Collection[]; busy: boolean; onRemove: (name: string) => void }) {
  const { t } = useText()
  if (collections.length === 0) return <p className="empty">{t('knowledge.emptyCollections')}</p>
  return <ul className="resource-list">{collections.map((collection) => <li key={collection.name}>
    <div className="row"><h2>{collection.name}</h2><button type="button" className="danger" disabled={busy} onClick={() => onRemove(collection.name)}>{t('knowledge.delete')}</button></div>
    {collection.description && <p>{collection.description}</p>}
    <p className="muted">{t('knowledge.groups')}: {collection.visibility.groups.join(', ') || t('knowledge.everyone')}</p>
  </li>)}</ul>
}
