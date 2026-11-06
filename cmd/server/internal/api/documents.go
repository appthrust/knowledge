package api

import (
	"strconv"
	"strings"

	"github.com/appthrust/appserver-go/appserver/httpx"
	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/appthrust/knowledge/cmd/server/internal/store"
)

func (a *API) collections(s *request) (any, error) {
	var out []domain.Collection
	err := a.store.Do(s.r.Context(), false, func(tx *store.Tx) error { var err error; out, err = tx.Collections(s.scope); return err })
	return httpx.Data(out), err
}
func (a *API) putCollection(s *request) (any, error) {
	var c domain.Collection
	if err := decode(s, &c); err != nil {
		return nil, err
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" || len(c.Name) > 128 || strings.ContainsAny(c.Name, "/\x00") {
		return nil, httpx.Invalid("name must contain 1 to 128 characters without slash or NUL")
	}
	if len(c.Description) > 4096 || len(c.Visibility.Groups) > 100 {
		return nil, httpx.Invalid("Collection description or groups exceed the size limit")
	}
	if c.Visibility.Groups == nil {
		c.Visibility.Groups = []string{}
	}
	err := a.store.Do(s.r.Context(), true, func(tx *store.Tx) error {
		// Editors may manage visible collections, but cannot rewrite hidden ones.
		all, err := tx.Collections(domain.Scope{Admin: true})
		if err != nil {
			return err
		}
		for _, existing := range all {
			if existing.Name == c.Name && !s.scope.Visible(existing) {
				return httpx.NotFound()
			}
		}
		if !s.scope.Visible(c) {
			return httpx.Forbidden()
		}
		return tx.PutCollection(c)
	})
	return c, err
}
func (a *API) deleteCollection(s *request) (any, error) {
	err := a.store.Do(s.r.Context(), true, func(tx *store.Tx) error {
		if _, err := tx.Collection(s.r.PathValue("name"), s.scope); err != nil {
			return err
		}
		return tx.DeleteCollection(s.r.PathValue("name"))
	})
	return map[string]bool{"deleted": true}, err
}
func (a *API) documents(s *request) (any, error) {
	params := s.r.URL.Query()
	limit := 100
	if raw := params.Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 500 {
			return nil, httpx.Invalid("limit must be between 1 and 500")
		}
	}
	var out []domain.Document
	err := a.store.Do(s.r.Context(), false, func(tx *store.Tx) error {
		var err error
		out, err = tx.Documents(params.Get("collection"), limit, s.scope)
		return err
	})
	return httpx.Data(out), err
}
func (a *API) document(s *request) (any, error) {
	var out domain.Document
	err := a.store.Do(s.r.Context(), false, func(tx *store.Tx) error {
		var err error
		out, err = tx.Document(s.r.PathValue("id"), s.scope)
		return err
	})
	return out, err
}
func (a *API) putDocument(s *request) (any, error) {
	var input struct {
		Collection string         `json:"collection"`
		ExternalID string         `json:"externalId"`
		Title      string         `json:"title"`
		URL        string         `json:"url"`
		Content    string         `json:"content"`
		Metadata   map[string]any `json:"metadata"`
	}
	if err := decode(s, &input); err != nil {
		return nil, err
	}
	if input.Collection == "" || strings.TrimSpace(input.Title) == "" || strings.TrimSpace(input.Content) == "" || len(input.Content) > domain.MaxContentBytes || len(input.Title) > 4096 || len(input.ExternalID) > 1024 || len(input.URL) > 8192 {
		return nil, httpx.Invalid("collection, title and content are required; content must not exceed 1 MiB")
	}
	if err := a.store.Do(s.r.Context(), false, func(tx *store.Tx) error { _, err := tx.Collection(input.Collection, s.scope); return err }); err != nil {
		return nil, err
	}
	chunks, err := a.options.Embedding.Chunks(s.r.Context(), input.Title, input.Content)
	if err != nil {
		return nil, httpx.Fail(502, "knowledge_embedding_unavailable", "The embedding provider could not process this document")
	}
	d := domain.Document{Collection: input.Collection, ExternalID: input.ExternalID, Title: input.Title, URL: input.URL, Content: input.Content, Metadata: input.Metadata}
	err = a.store.Do(s.r.Context(), true, func(tx *store.Tx) error {
		if _, err := tx.Collection(input.Collection, s.scope); err != nil {
			return err
		}
		var err error
		d, err = tx.PutDocument(d, chunks, a.options.Embedding.IndexProvider())
		return err
	})
	return map[string]any{"id": d.ID, "collection": d.Collection, "chunks": d.Chunks}, err
}
func (a *API) deleteDocument(s *request) (any, error) {
	err := a.store.Do(s.r.Context(), true, func(tx *store.Tx) error { return tx.DeleteDocument(s.r.PathValue("id"), s.scope) })
	return map[string]bool{"deleted": true}, err
}
