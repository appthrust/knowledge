package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/appthrust/appserver-go/appserver/httpx"
	"github.com/appthrust/appserver-go/appserver/identity"
	"github.com/appthrust/appserver-go/appserver/platformclient"
	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/appthrust/knowledge/cmd/server/internal/store"
)

type Options struct {
	ProjectID   string
	Embedding   *domain.Embedder
	LLM         *domain.LLM
	Notion      *domain.Notion
	NotionToken string
	Esa         *domain.Esa
	EsaToken    string
	Health      *HealthEvidence
}

// managedToken is the platform-managed credential for a source type, empty
// when the Project has not bound one.
func (o Options) managedToken(sourceType string) string {
	switch sourceType {
	case "notion":
		return o.NotionToken
	case "esa":
		return o.EsaToken
	}
	return ""
}

type API struct {
	store    *store.Store
	identity *identity.Authenticator
	platform *platformclient.Client
	options  Options
	mux      *http.ServeMux
	now      func() time.Time
	esa      esaConnectionCache
}
type request struct {
	w     http.ResponseWriter
	r     *http.Request
	scope domain.Scope
}
type endpoint func(*request) (any, error)

func New(database *store.Store, auth *identity.Authenticator, platform *platformclient.Client, options Options) http.Handler {
	a := &API{store: database, identity: auth, platform: platform, options: options, mux: http.NewServeMux(), now: time.Now}
	a.route("GET /api/knowledge/v1/openapi.json", false, false, a.openAPI)
	a.route("GET /api/knowledge/v1/collections", false, false, a.collections)
	a.route("POST /api/knowledge/v1/collections", false, true, a.putCollection)
	a.route("DELETE /api/knowledge/v1/collections/{name}", true, true, a.deleteCollection)
	a.route("GET /api/knowledge/v1/documents", false, false, a.documents)
	a.route("POST /api/knowledge/v1/documents", false, true, a.putDocument)
	a.route("GET /api/knowledge/v1/documents/{id}", false, false, a.document)
	a.route("DELETE /api/knowledge/v1/documents/{id}", false, true, a.deleteDocument)
	a.route("GET /api/knowledge/v1/sources", true, false, a.sources)
	a.route("POST /api/knowledge/v1/sources", true, true, a.putSource)
	a.route("GET /api/knowledge/v1/sources/{id}", true, false, a.source)
	a.route("POST /api/knowledge/v1/sources/{id}/sync", true, true, a.sync)
	a.route("DELETE /api/knowledge/v1/sources/{id}", true, true, a.deleteSource)
	a.route("GET /api/knowledge/v1/jobs/{id}", false, false, a.job)
	a.route("GET /api/knowledge/v1/providers/esa", true, false, a.esaConnection)
	a.route("GET /api/knowledge/v1/search", false, false, a.search)
	a.route("POST /api/knowledge/v1/search", false, false, a.search)
	a.route("POST /api/knowledge/v1/answer", false, false, a.answer)
	a.route("/", true, false, func(*request) (any, error) { return nil, httpx.NotFound() })
	return a
}
func Unavailable() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, httpx.Fail(503, "knowledge_store_unavailable", "The knowledge database or its vector extension is not available"), "knowledge")
	})
}
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a.mux.ServeHTTP(w, r)
}
func (a *API) route(pattern string, management, mutation bool, handler endpoint) {
	timeout := 2 * time.Minute
	if pattern == "POST /api/knowledge/v1/answer" {
		// Allow the gateway's 300-second header wait plus discovery and streaming.
		timeout = 330 * time.Second
	}
	a.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		r = r.WithContext(ctx)
		scope, err := a.authenticate(r, management, mutation)
		if err != nil {
			httpx.WriteError(w, err, "knowledge")
			return
		}
		body, err := handler(&request{w: w, r: r, scope: scope})
		a.recordHealth(pattern, body, err)
		if err != nil {
			httpx.WriteError(w, err, "knowledge")
			return
		}
		if _, ok := body.(streamed); ok {
			return
		}
		httpx.WriteJSON(w, 200, body)
	})
}
func decode(s *request, target any) error {
	media, _, err := mime.ParseMediaType(s.r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return httpx.Invalid("Content-Type must be application/json")
	}
	body, err := io.ReadAll(http.MaxBytesReader(s.w, s.r.Body, 8<<20))
	if err != nil {
		return httpx.Invalid("Request body exceeds the size limit")
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return httpx.Invalid("Request body must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return httpx.Invalid("Request body contains invalid or unknown fields")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return httpx.Invalid("Request body must contain one JSON object")
	}
	return nil
}
