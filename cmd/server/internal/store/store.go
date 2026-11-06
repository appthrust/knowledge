package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/appthrust/appserver-go/appserver/httpx"
	"github.com/appthrust/appserver-go/appserver/pgstore"
	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrations embed.FS
var ErrVectorPending = errors.New("knowledge database extension vector is not yet provisioned")

type Store struct{ *pgstore.Store }
type Tx struct{ *pgstore.Tx }

func Open(ctx context.Context, dsn string) (*Store, error) {
	// CNPG installs vector using its privileged Database reconciler. Dev databases
	// may grant superuser; only insufficient_privilege is a pending precondition.
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var exists bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='vector')`).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		_, err = db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public`)
		if err != nil {
			var pg *pq.Error
			if !errors.As(err, &pg) || pg.Code != "42501" {
				return nil, err
			}
		}
		if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='vector')`).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrVectorPending
		}
	}
	database, err := pgstore.Open(ctx, dsn, migrations, "knowledge_schema_migrations")
	if err != nil {
		return nil, err
	}
	s := &Store{database}
	if err := s.reindexSearchText(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.Do(ctx, true, func(tx *Tx) error {
		return tx.Exec(`UPDATE ingest_jobs SET state='queued' WHERE state='running'`)
	}); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// reindexSearchText tokenises chunks that predate migration 003: rows whose
// search_text is empty while their text is not. A later tokenizer change
// must ship a migration that clears search_text so this runs again.
func (s *Store) reindexSearchText(ctx context.Context) error {
	return s.Do(ctx, true, func(tx *Tx) error {
		rows, err := tx.SQL.QueryContext(tx.Context, `SELECT c.document_id,c.chunk_index,d.title||' '||c.text FROM chunks c JOIN documents d ON d.id=c.document_id WHERE c.search_text='' AND c.text<>''`)
		if err != nil {
			return err
		}
		type pending struct {
			id    string
			index int
			text  string
		}
		var todo []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.index, &p.text); err != nil {
				rows.Close()
				return err
			}
			todo = append(todo, p)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range todo {
			if err := tx.Exec(`UPDATE chunks SET search_text=$3 WHERE document_id=$1 AND chunk_index=$2`, p.id, p.index, domain.SearchText(p.text)); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Store) Do(ctx context.Context, write bool, fn func(*Tx) error) error {
	err := s.Store.Do(ctx, write, func(tx *pgstore.Tx) error { return fn(&Tx{tx}) })
	if errors.Is(err, sql.ErrNoRows) {
		return httpx.NotFound()
	}
	var pg *pq.Error
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return httpx.Conflict("This resource already exists")
		case "23503", "23514":
			return httpx.Invalid("A referenced resource or value is invalid")
		}
	}
	return err
}
func (t *Tx) Collections(scope domain.Scope) ([]domain.Collection, error) {
	rows, err := t.SQL.QueryContext(t.Context, `SELECT name,description,visibility_groups FROM collections ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Collection{}
	for rows.Next() {
		var c domain.Collection
		if err := rows.Scan(&c.Name, &c.Description, pq.Array(&c.Visibility.Groups)); err != nil {
			return nil, err
		}
		if scope.Visible(c) {
			result = append(result, c)
		}
	}
	return result, rows.Err()
}
func (t *Tx) Collection(name string, scope domain.Scope) (domain.Collection, error) {
	var c domain.Collection
	err := t.SQL.QueryRowContext(t.Context, `SELECT name,description,visibility_groups FROM collections WHERE name=$1`, name).Scan(&c.Name, &c.Description, pq.Array(&c.Visibility.Groups))
	if err == nil && !scope.Visible(c) {
		err = httpx.NotFound()
	}
	return c, err
}
func (t *Tx) PutCollection(c domain.Collection) error {
	return t.Exec(`INSERT INTO collections(name,description,visibility_groups) VALUES($1,$2,$3) ON CONFLICT(name) DO UPDATE SET description=excluded.description,visibility_groups=excluded.visibility_groups`, c.Name, c.Description, pq.Array(c.Visibility.Groups))
}
func (t *Tx) DeleteCollection(name string) error {
	res, err := t.SQL.ExecContext(t.Context, `DELETE FROM collections WHERE name=$1`, name)
	return affected(res, err)
}
func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}
func (t *Tx) PutDocument(d domain.Document, chunks []domain.Chunk, provider string) (domain.Document, error) {
	if d.ID == "" {
		d.ID = uuid.NewString()
	}
	if d.Metadata == nil {
		d.Metadata = map[string]any{}
	}
	meta, err := json.Marshal(d.Metadata)
	if err != nil {
		return d, err
	}
	err = t.SQL.QueryRowContext(t.Context, `INSERT INTO documents(id,collection,external_id,title,url,content,metadata) VALUES($1,$2,NULLIF($3,''),$4,$5,$6,$7) ON CONFLICT(collection,external_id) DO UPDATE SET title=excluded.title,url=excluded.url,content=excluded.content,metadata=excluded.metadata,updated_at=now() RETURNING id`, d.ID, d.Collection, d.ExternalID, d.Title, d.URL, d.Content, string(meta)).Scan(&d.ID)
	if err != nil {
		return d, err
	}
	if err = t.replaceChunks(d, chunks, provider); err != nil {
		return d, err
	}
	d.Chunks = len(chunks)
	return d, nil
}

func (t *Tx) replaceChunks(d domain.Document, chunks []domain.Chunk, provider string) error {
	if err := t.Exec(`DELETE FROM chunks WHERE document_id=$1`, d.ID); err != nil {
		return err
	}
	for i, c := range chunks {
		if err := t.Exec(`INSERT INTO chunks(document_id,chunk_index,text,search_text,embedding,embedding_dim,embedding_provider) VALUES($1,$2,$3,$4,$5::vector,$6,$7)`, d.ID, i, c.Text, domain.SearchText(d.Title+" "+c.Text), vector(c.Embedding), len(c.Embedding), provider); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tx) ReembedPending(provider string) (int, error) {
	var count int
	err := t.SQL.QueryRowContext(t.Context, `SELECT count(DISTINCT document_id) FROM chunks WHERE embedding_provider<>$1`, provider).Scan(&count)
	return count, err
}

// ReembedCandidates bounds memory to eight documents. Converted chunks are
// the checkpoint, so a restarted job never repeats completed documents. Newest
// documents convert first: a freshly synced source serves vector search hours
// before stale ones that are about to be pruned or deleted.
func (t *Tx) ReembedCandidates(provider string) ([]domain.Document, error) {
	rows, err := t.SQL.QueryContext(t.Context, documentSelect+` WHERE EXISTS(SELECT 1 FROM chunks c WHERE c.document_id=d.id AND c.embedding_provider<>$1) ORDER BY d.updated_at DESC,d.id LIMIT 8`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Document
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ReplaceEmbeddings never overwrites a concurrent document edit or resurrects
// a deleted document. An edited candidate is picked up on the next batch.
func (t *Tx) ReplaceEmbeddings(d domain.Document, chunks []domain.Chunk, provider string) (bool, error) {
	var title, content string
	err := t.SQL.QueryRowContext(t.Context, `SELECT title,content FROM documents WHERE id=$1 FOR UPDATE`, d.ID).Scan(&title, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || title != d.Title || content != d.Content {
		return false, err
	}
	return true, t.replaceChunks(d, chunks, provider)
}
func vector(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, n := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(n), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

const documentSelect = `SELECT d.id,d.collection,COALESCE(d.external_id,''),d.title,d.url,d.content,d.metadata,(SELECT count(*) FROM chunks WHERE document_id=d.id) FROM documents d`

func scanDocument(row interface{ Scan(...any) error }) (domain.Document, error) {
	var d domain.Document
	var meta []byte
	err := row.Scan(&d.ID, &d.Collection, &d.ExternalID, &d.Title, &d.URL, &d.Content, &meta, &d.Chunks)
	if err == nil {
		err = json.Unmarshal(meta, &d.Metadata)
	}
	return d, err
}

// Intros returns the first `runes` characters of each document's content:
// a handbook post opens with its purpose ("ここで得られる知識"), which the
// reranker needs alongside the matched chunk. Visibility was already
// enforced when the hits were retrieved.
func (t *Tx) Intros(ids []string, runes int) (map[string]string, error) {
	rows, err := t.SQL.QueryContext(t.Context, `SELECT id,left(content,$2) FROM documents WHERE id=ANY($1)`, pq.Array(ids), runes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, intro string
		if err := rows.Scan(&id, &intro); err != nil {
			return nil, err
		}
		out[id] = intro
	}
	return out, rows.Err()
}

func (t *Tx) Document(id string, scope domain.Scope) (domain.Document, error) {
	d, err := scanDocument(t.SQL.QueryRowContext(t.Context, documentSelect+` WHERE d.id=$1`, id))
	if err == nil {
		_, err = t.Collection(d.Collection, scope)
	}
	return d, err
}
func (t *Tx) Documents(collection string, limit int, scope domain.Scope) ([]domain.Document, error) {
	collections, err := t.Collections(scope)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, c := range collections {
		if collection == "" || c.Name == collection {
			names = append(names, c.Name)
		}
	}
	rows, err := t.SQL.QueryContext(t.Context, documentSelect+` WHERE d.collection=ANY($1) ORDER BY d.updated_at DESC,d.id LIMIT $2`, pq.Array(names), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Document{}
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
func (t *Tx) DeleteDocument(id string, scope domain.Scope) error {
	if _, err := t.Document(id, scope); err != nil {
		return err
	}
	return t.Exec(`DELETE FROM documents WHERE id=$1`, id)
}
func (t *Tx) Search(q domain.Search, embedding []float32, provider string, scope domain.Scope) ([]domain.Hit, error) {
	collections, err := t.Collections(scope)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, c := range collections {
		if len(q.Collections) == 0 {
			names = append(names, c.Name)
		} else {
			for _, name := range q.Collections {
				if c.Name == name {
					names = append(names, c.Name)
					break
				}
			}
		}
	}
	// Materialize the filtered relation before any vector distance calculation:
	// mixed provider dimensions coexist safely in this untyped-vector table.
	base := `WITH eligible AS MATERIALIZED (SELECT c.*,d.title,d.url,d.collection,d.metadata FROM chunks c JOIN documents d ON d.id=c.document_id WHERE d.collection=ANY($1))`
	// Fusion is per document: each leg contributes a reciprocal-rank score
	// for a document's best chunk, so a long document with many matching
	// chunks cannot crowd the result list, and its best chunk is shown.
	// Reciprocal rank is used in every mode so scores stay comparable and the
	// popularity prior (connector watchers/stars) can be added uniformly.
	limit := max(q.TopK*4, 50)
	type ranked struct {
		domain.Hit
		best float64
	}
	docs := map[string]*ranked{}
	for _, mode := range []string{"lexical", "vector"} {
		if q.Mode != "hybrid" && q.Mode != mode {
			continue
		}
		var query string
		var args []any
		weight := 1.0
		if mode == "lexical" {
			// Strict plan (every term as a phrase) first. In lexical mode an
			// empty result falls back to the loose any-token plan at half
			// weight so a keyword search never returns nothing; in hybrid the
			// fallback is skipped because its bigram noise outranked the vector
			// leg (handbook benchmark: hybrid Recall@3 0.63 → 0.82).
			strict, loose := domain.SearchQueries(q.Query)
			if strict == "" {
				continue
			}
			query = base + ` SELECT document_id,title,url,collection,metadata,chunk_index,text,ts_rank_cd(search_vector,query,32)::float8 AS score FROM eligible, to_tsquery('simple',$2) query WHERE search_vector @@ query ORDER BY score DESC,document_id,chunk_index LIMIT $3`
			args = []any{pq.Array(names), strict, limit}
			var n int
			if err := t.SQL.QueryRowContext(t.Context, base+` SELECT count(*) FROM eligible, to_tsquery('simple',$2) query WHERE search_vector @@ query`, pq.Array(names), strict).Scan(&n); err != nil {
				return nil, err
			}
			if n == 0 {
				if q.Mode == "hybrid" {
					continue
				}
				args[1], weight = loose, 0.5
			}
		} else {
			query = base + `, compatible AS MATERIALIZED (SELECT * FROM eligible WHERE embedding_provider=$2 AND embedding_dim=$3) SELECT document_id,title,url,collection,metadata,chunk_index,text,(1-(embedding <=> $4::vector))::float8 AS score FROM compatible ORDER BY embedding <=> $4::vector,document_id,chunk_index LIMIT $5`
			args = []any{pq.Array(names), provider, len(embedding), vector(embedding), limit}
		}
		rows, err := t.SQL.QueryContext(t.Context, query, args...)
		if err != nil {
			return nil, err
		}
		rank := 0
		seen := map[string]bool{}
		for rows.Next() {
			var h domain.Hit
			var meta []byte
			var raw float64
			if err := rows.Scan(&h.DocumentID, &h.Title, &h.URL, &h.Collection, &meta, &h.ChunkIndex, &h.Text, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			if seen[h.DocumentID] {
				continue // only a document's best chunk per leg counts
			}
			seen[h.DocumentID] = true
			rank++
			score := weight / float64(60+rank)
			if existing := docs[h.DocumentID]; existing != nil {
				existing.Score += score
				if raw > existing.best {
					existing.best, existing.ChunkIndex, existing.Text = raw, h.ChunkIndex, h.Text
				}
				continue
			}
			_ = json.Unmarshal(meta, &h.Metadata)
			h.Score = score
			docs[h.DocumentID] = &ranked{Hit: h, best: raw}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	hits := make([]domain.Hit, 0, len(docs))
	for _, d := range docs {
		// Adjacent ranks differ by about 1/61-1/62; the prior tops out at
		// that gap so popularity only reorders near-ties, never a clear match.
		d.Score += domain.Popularity(d.Metadata) * (1.0/61 - 1.0/62)
		hits = append(hits, d.Hit)
	}
	sortHits(hits)
	if len(hits) > q.TopK {
		hits = hits[:q.TopK]
	}
	return hits, nil
}
