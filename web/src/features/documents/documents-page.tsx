import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { knowledgeApi } from '../../api/knowledge'
import { collectionsQuery, documentsQuery, invalidateKnowledge } from '../../api/queries'
import type { DocumentInput, KnowledgeDocument } from '../../api/types'
import { useText } from '../../i18n'
import { CollectionSelect, ErrorNotice, ExternalLink, Field, formText, PageHeading, QueryFeedback } from '../../ui'

export function DocumentsPage() {
  const { t } = useText()
  const client = useQueryClient()
  const [selected, setSelected] = useState('')
  const collections = useQuery(collectionsQuery)
  const collection = selected || collections.data?.[0]?.name || ''
  const documents = useQuery(documentsQuery(collection))
  const create = useMutation({ mutationFn: (input: DocumentInput) => {
    if (new TextEncoder().encode(input.content).length > 1024 * 1024) throw new Error(t('knowledge.tooLarge'))
    return knowledgeApi.documents.create(input)
  }, onSuccess: () => invalidateKnowledge(client) })
  const remove = useMutation({ mutationFn: knowledgeApi.documents.remove, onSuccess: () => invalidateKnowledge(client) })
  return <>
    <PageHeading title={t('knowledge.documents')}>{t('knowledge.documentsHelp')}</PageHeading>
    <QueryFeedback pending={collections.isPending} error={collections.error} retry={collections.refetch} />
    {collections.data && <CollectionSelect collections={collections.data} value={collection} onChange={setSelected} />}
    {collections.data?.length === 0 && <p className="empty">{t('knowledge.emptyCollections')}</p>}
    {collection && <div className="workspace-grid">
      <section aria-label={t('knowledge.documents')}>
        <QueryFeedback pending={documents.isPending} error={documents.error} retry={documents.refetch} />
        <ErrorNotice error={remove.error} />
        {documents.data && <DocumentsList documents={documents.data} busy={remove.isPending} onRemove={(document) => { if (window.confirm(t('knowledge.deleteDocument', { name: document.title }))) remove.mutate(document.id) }} />}
      </section>
      <form className="editor" onSubmit={(event) => {
        event.preventDefault()
        const form = event.currentTarget
        const data = new FormData(form)
        create.mutate({ collection, title: formText(data, 'title'), url: formText(data, 'url'), content: String(data.get('content') ?? '') }, { onSuccess: () => form.reset() })
      }}>
        <h2>{t('knowledge.addDocument')}</h2>
        <Field label={t('knowledge.title')}><input name="title" required /></Field>
        <Field label={t('knowledge.url')}><input name="url" type="url" /></Field>
        <Field label={t('knowledge.content')} hint={t('knowledge.contentLimit')}><textarea name="content" required rows={12} className="markdown-input" /></Field>
        <ErrorNotice error={create.error} />
        {create.data && <p role="status">{t('knowledge.chunks', { count: create.data.chunks })}</p>}
        <button disabled={create.isPending}>{t(create.isPending ? 'knowledge.saving' : 'knowledge.addDocument')}</button>
      </form>
    </div>}
  </>
}
export function DocumentsList({ documents, busy, onRemove }: { documents: KnowledgeDocument[]; busy: boolean; onRemove: (document: KnowledgeDocument) => void }) {
  const { t } = useText()
  if (documents.length === 0) return <p className="empty">{t('knowledge.emptyDocuments')}</p>
  return <ul className="resource-list">{documents.map((document) => <li key={document.id}>
    <div className="row"><h2><ExternalLink url={document.url}>{document.title}</ExternalLink></h2><button type="button" className="danger" disabled={busy} onClick={() => onRemove(document)}>{t('knowledge.delete')}</button></div>
    <p className="muted">{t('knowledge.chunks', { count: document.chunks })}</p>
    {document.url && <p className="mono muted">{document.url}</p>}
  </li>)}</ul>
}
