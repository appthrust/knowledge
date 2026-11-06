package store

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"

	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

func sortHits(hits []domain.Hit) {
	slices.SortFunc(hits, func(a, b domain.Hit) int {
		if n := cmp.Compare(b.Score, a.Score); n != 0 {
			return n
		}
		if n := cmp.Compare(a.DocumentID, b.DocumentID); n != 0 {
			return n
		}
		return cmp.Compare(a.ChunkIndex, b.ChunkIndex)
	})
}
func (t *Tx) PutSource(s domain.Source) (domain.Source, error) {
	s.ID = uuid.NewString()
	config, err := json.Marshal(s.Config)
	if err == nil {
		err = t.Exec(`INSERT INTO sources(id,type,collection,config) VALUES($1,$2,$3,$4)`, s.ID, s.Type, s.Collection, string(config))
	}
	return s, err
}
func scanSource(row interface{ Scan(...any) error }) (domain.Source, error) {
	var s domain.Source
	var config, last []byte
	err := row.Scan(&s.ID, &s.Type, &s.Collection, &config, &last)
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal(config, &s.Config); err != nil {
		return s, err
	}
	if len(last) > 0 {
		err = json.Unmarshal(last, &s.LastSync)
	}
	return s, err
}
func (t *Tx) Source(id string) (domain.Source, error) {
	source, err := scanSource(t.SQL.QueryRowContext(t.Context, `SELECT id,type,collection,config,last_sync FROM sources WHERE id=$1`, id))
	if err == nil {
		source.Sync, err = t.LatestSync(id)
	}
	return source, err
}
func (t *Tx) Sources() ([]domain.Source, error) {
	rows, err := t.SQL.QueryContext(t.Context, `SELECT id,type,collection,config,last_sync FROM sources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Source{}
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		s.Config.Token = ""
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		out[i].Sync, err = t.LatestSync(out[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (t *Tx) Unchanged(sourceID, externalID, edited string) (bool, error) {
	var same bool
	err := t.SQL.QueryRowContext(t.Context, `SELECT EXISTS(SELECT 1 FROM source_revisions WHERE source_id=$1 AND external_id=$2 AND edited=$3)`, sourceID, externalID, edited).Scan(&same)
	return same && edited != "", err
}
func (t *Tx) Revision(sourceID, externalID, edited, documentID string) error {
	return t.Exec(`INSERT INTO source_revisions(source_id,external_id,edited,document_id) VALUES($1,$2,$3,$4) ON CONFLICT(source_id,external_id) DO UPDATE SET edited=excluded.edited,document_id=excluded.document_id`, sourceID, externalID, edited, documentID)
}

// Prune deletes the source's documents whose external ID the completed walk
// did not return, so the index mirrors the source's current snapshot. Chunks
// and revisions follow by cascade. An empty walk prunes everything.
func (t *Tx) Prune(sourceID string, seen []string) (int, error) {
	if seen == nil {
		seen = []string{}
	}
	res, err := t.SQL.ExecContext(t.Context, `DELETE FROM documents WHERE id IN (SELECT document_id FROM source_revisions WHERE source_id=$1 AND NOT (external_id = ANY($2)))`, sourceID, pq.Array(seen))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// DeleteSource removes the source with every document it ingested and reports
// how many; jobs and revisions follow by cascade. Manually posted documents are
// never touched.
func (t *Tx) DeleteSource(id string) (int, error) {
	res, err := t.SQL.ExecContext(t.Context, `DELETE FROM documents WHERE id IN (SELECT document_id FROM source_revisions WHERE source_id=$1)`, id)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), affected(t.SQL.ExecContext(t.Context, `DELETE FROM sources WHERE id=$1`, id))
}
func (t *Tx) SyncResult(id string, result domain.SyncResult) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return t.Exec(`UPDATE sources SET last_sync=$2 WHERE id=$1`, id, string(body))
}
func (s *Store) SeedDev(ctx context.Context, e *domain.Embedder) error {
	return s.Do(ctx, true, func(tx *Tx) error {
		collections, err := tx.Collections(domain.Scope{Admin: true})
		if err != nil || len(collections) > 0 {
			return err
		}
		if err = tx.PutCollection(domain.Collection{Name: "getting-started", Description: "Knowledge demo documents", Visibility: domain.Visibility{Groups: []string{}}}); err != nil {
			return err
		}
		docs := []domain.Document{
			{Title: "Welcome to Knowledge", Content: "# Knowledge\n\nSearch and ask across your own Markdown documents. Hybrid retrieval combines lexical and vector search."},
			{Title: "Connect an application", Content: "# Component connections\n\nConnect an application to Knowledge to receive KNOWLEDGE_URL and KNOWLEDGE_API_KEY. Push documents and search from your app."},
			{Title: "Notion collections", Content: "# Notion\n\nCreate a Notion source with an integration token. Sync pages into a collection. Group visibility restricts document access."},
		}
		for i, d := range docs {
			d.Collection = "getting-started"
			d.ExternalID = string(rune('1' + i))
			chunks, err := e.Chunks(ctx, d.Title, d.Content)
			if err != nil {
				return err
			}
			if _, err = tx.PutDocument(d, chunks, e.IndexProvider()); err != nil {
				return err
			}
		}
		return nil
	})
}
