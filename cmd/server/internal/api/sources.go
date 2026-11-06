package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/appthrust/appserver-go/appserver/httpx"
	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/appthrust/knowledge/cmd/server/internal/store"
)

func (a *API) sources(s *request) (any, error) {
	out := []domain.Source{}
	err := a.store.Do(s.r.Context(), false, func(tx *store.Tx) error {
		items, err := tx.Sources()
		if err != nil {
			return err
		}
		collections, err := tx.Collections(s.scope)
		if err != nil {
			return err
		}
		for _, item := range items {
			for _, c := range collections {
				if item.Collection == c.Name {
					out = append(out, a.sourceView(item))
					break
				}
			}
		}
		return nil
	})
	return httpx.Data(out), err
}
func (a *API) source(s *request) (any, error) {
	var out domain.Source
	err := a.store.Do(s.r.Context(), false, func(tx *store.Tx) error {
		var err error
		out, err = tx.Source(s.r.PathValue("id"))
		if err != nil {
			return err
		}
		_, err = tx.Collection(out.Collection, s.scope)
		return err
	})
	return a.sourceView(out), err
}
func (a *API) putSource(s *request) (any, error) {
	var input struct {
		Type       string `json:"type"`
		Collection string `json:"collection"`
		Config     struct {
			Token           json.RawMessage `json:"token"`
			RootPageIDs     []string        `json:"rootPageIds,omitempty"`
			DatabaseIDs     []string        `json:"databaseIds,omitempty"`
			Team            string          `json:"team,omitempty"`
			Query           string          `json:"query,omitempty"`
			IncludeWIP      bool            `json:"includeWip,omitempty"`
			IncludeArchived bool            `json:"includeArchived,omitempty"`
		} `json:"config"`
	}
	if err := decode(s, &input); err != nil {
		return nil, err
	}
	if input.Type != "notion" && input.Type != "esa" {
		return nil, httpx.Invalid("Source type must be notion or esa")
	}
	managed := a.options.managedToken(input.Type) != ""
	if managed && input.Config.Token != nil {
		return nil, httpx.Fail(400, "knowledge_"+input.Type+"_token_platform_managed", "The "+providerName(input.Type)+" token is managed in the Project's built-in app settings")
	}
	token := ""
	if input.Config.Token != nil && json.Unmarshal(input.Config.Token, &token) != nil {
		return nil, httpx.Invalid(providerName(input.Type) + " token must be a string")
	}
	if input.Collection == "" || (!managed && strings.TrimSpace(token) == "") || len(token) > 8192 {
		return nil, httpx.Invalid("A source type, collection and token are required")
	}
	out := domain.Source{Type: input.Type, Collection: input.Collection, Config: domain.SourceConfig{Token: token}}
	switch input.Type {
	case "notion":
		if len(input.Config.RootPageIDs)+len(input.Config.DatabaseIDs) > 100 {
			return nil, httpx.Invalid("A notion source accepts at most 100 roots")
		}
		for _, ids := range [][]string{input.Config.RootPageIDs, input.Config.DatabaseIDs} {
			for _, id := range ids {
				if id == "" || len(id) > 128 || strings.ContainsAny(id, "/?#") {
					return nil, httpx.Invalid("Invalid Notion page or database ID")
				}
			}
		}
		out.Config.RootPageIDs, out.Config.DatabaseIDs = input.Config.RootPageIDs, input.Config.DatabaseIDs
	case "esa":
		if !domain.ValidEsaTeam(input.Config.Team) {
			return nil, httpx.Invalid("An esa team must be the lowercase subdomain of https://<team>.esa.io")
		}
		if len(input.Config.Query) > 1024 || strings.ContainsAny(input.Config.Query, "\n\r") {
			return nil, httpx.Invalid("An esa query must be a single line of at most 1024 characters")
		}
		out.Config.Team, out.Config.Query, out.Config.IncludeWIP, out.Config.IncludeArchived = input.Config.Team, strings.TrimSpace(input.Config.Query), input.Config.IncludeWIP, input.Config.IncludeArchived
	}
	err := a.store.Do(s.r.Context(), true, func(tx *store.Tx) error {
		if _, err := tx.Collection(out.Collection, s.scope); err != nil {
			return err
		}
		var err error
		out, err = tx.PutSource(out)
		return err
	})
	return a.sourceView(out), err
}
func providerName(sourceType string) string {
	if sourceType == "notion" {
		return "Notion"
	}
	return sourceType
}
func (a *API) sync(s *request) (any, error) {
	var job domain.IngestJob
	err := a.store.Do(s.r.Context(), true, func(tx *store.Tx) error {
		source, err := tx.Source(s.r.PathValue("id"))
		if err != nil {
			return err
		}
		if _, err = tx.Collection(source.Collection, s.scope); err != nil {
			return err
		}
		job, err = tx.EnqueueSync(source.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	httpx.WriteJSON(s.w, http.StatusAccepted, map[string]any{"job": job})
	return streamed{}, nil
}

// deleteSource refuses while the source's sync job is queued or running: the
// worker would otherwise re-create documents that no source can reach.
func (a *API) deleteSource(s *request) (any, error) {
	documents := 0
	err := a.store.Do(s.r.Context(), true, func(tx *store.Tx) error {
		source, err := tx.Source(s.r.PathValue("id"))
		if err != nil {
			return err
		}
		if _, err = tx.Collection(source.Collection, s.scope); err != nil {
			return err
		}
		if source.Sync != nil && (source.Sync.State == "queued" || source.Sync.State == "running") {
			return httpx.Fail(409, "knowledge_source_syncing", "The source is being synced; wait for the sync to finish")
		}
		documents, err = tx.DeleteSource(source.ID)
		return err
	})
	return map[string]any{"deleted": true, "documents": documents}, err
}

func (a *API) sourceView(source domain.Source) domain.Source {
	source.Config.Token = ""
	source.TokenSource = "stored"
	if a.options.managedToken(source.Type) != "" {
		source.TokenSource = "platform"
	}
	return source
}
