import type { ReactNode } from 'react'
import type { Collection } from '../api/types'
import { useText } from '../i18n'

export function PageHeading({ title, children }: { title: string; children: ReactNode }) {
  return <div className="page-heading"><h1>{title}</h1><p>{children}</p></div>
}
export function Field({ label, children, hint }: { label: string; children: ReactNode; hint?: string }) {
  return <label className="field"><span>{label}</span>{children}{hint && <small>{hint}</small>}</label>
}
export function ErrorNotice({ error }: { error: unknown }) {
  const { t } = useText()
  if (!error) return null
  return <div className="notice error" role="alert"><strong>{t('knowledge.error')}</strong>{error instanceof Error && <p>{error.message}</p>}</div>
}
export function QueryFeedback({ pending, error, retry }: { pending: boolean; error: unknown; retry: () => unknown }) {
  const { t } = useText()
  if (pending) return <p role="status">{t('knowledge.loading')}</p>
  if (!error) return null
  return <><ErrorNotice error={error} /><button type="button" className="secondary" onClick={() => { void retry() }}>{t('knowledge.retry')}</button></>
}
export function CollectionSelect({ collections, value, onChange }: { collections: Collection[]; value: string; onChange: (value: string) => void }) {
  const { t } = useText()
  return <Field label={t('knowledge.collection')}><select name="collection" required value={value} onChange={(event) => onChange(event.target.value)}><option value="">{t('knowledge.chooseCollection')}</option>{collections.map((collection) => <option key={collection.name} value={collection.name}>{collection.name}</option>)}</select></Field>
}
export function ExternalLink({ url, children }: { url?: string; children: ReactNode }) {
  // Document URLs are untrusted content, not navigation instructions.
  if (!url || !/^https?:\/\//i.test(url)) return <>{children}</>
  return <a href={url} target="_blank" rel="noreferrer">{children}</a>
}
export const formText = (form: FormData, name: string) => String(form.get(name) ?? '').trim()
export const splitIds = (value: string) => [...new Set(value.split(/[,\n]/).map((id) => id.trim()).filter(Boolean))]
