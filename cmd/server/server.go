package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/appthrust/appserver-go/appserver/health"
	"github.com/appthrust/appserver-go/appserver/httpx"
	"github.com/appthrust/appserver-go/appserver/identity"
	"github.com/appthrust/appserver-go/appserver/launcher"
	"github.com/appthrust/appserver-go/appserver/pending"
	"github.com/appthrust/appserver-go/appserver/platformclient"
	"github.com/appthrust/appserver-go/appserver/platformenv"
	"github.com/appthrust/appserver-go/appserver/releaseidentity"
	"github.com/appthrust/knowledge/cmd/server/internal/api"
	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// parseOrigin validates the public URL before applying its HTTPS-only policy.
func parseOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("URL must be an origin without credentials, a path, query, or fragment")
	}
	return u, nil
}

func validateConfig(upstreamURL, projectID string) (api.Options, error) {
	if _, err := platformenv.Validate(upstreamURL, projectID); err != nil {
		return api.Options{}, err
	}
	return runtimeOptions(projectID)
}

// newHandler mounts the knowledge under the SPA. ready gates /readyz only
// (see pending.Probes).
func newHandler(staticDir, upstreamURL, projectID string, knowledge http.Handler, ready func() bool) (http.Handler, error) {
	options, err := validateConfig(upstreamURL, projectID)
	if err != nil {
		return nil, err
	}
	authenticator, err := identity.New(identity.FromEnv("KNOWLEDGE"))
	if err != nil {
		return nil, err
	}
	catalog := launcher.NewCatalog(platformclient.New(upstreamURL, projectID, platformclient.WithHTTPClient(authenticator.HTTPClient())), "knowledge")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		embedding, err := api.EmbeddingConfig(r.Context(), nil, options.Embedding)
		if live, ok := knowledge.(interface {
			EmbeddingConfig(context.Context) (domain.EmbeddingConfig, error)
		}); ok {
			embedding, err = live.EmbeddingConfig(r.Context())
		}
		if err != nil {
			httpx.WriteError(w, err, "knowledge")
			return
		}
		llm := map[string]any{"configured": options.LLM != nil}
		if options.LLM != nil {
			llm["provider"] = options.LLM.Provider
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"projectId": projectID, "publicUrl": os.Getenv("KNOWLEDGE_PUBLIC_URL"),
			"embedding": embedding,
			"llm":       llm,
			"notion":    map[string]bool{"platformManaged": options.NotionToken != ""},
			"esa":       map[string]bool{"platformManaged": options.EsaToken != ""},
		})
	})
	mux.Handle("GET /launcher", launcher.NewHandler(catalog, authenticator.Authenticate))
	mux.Handle("GET /releasez", releaseidentity.NewHandler("KNOWLEDGE", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("UI release status unavailable\n"))
	}))
	pending.Probes(mux, ready)
	if application, ok := knowledge.(interface{ HealthHandler() http.Handler }); ok {
		mux.Handle("GET "+health.Path, application.HealthHandler())
	}
	if knowledge == nil {
		knowledge = api.Unavailable()
	}
	mux.Handle("/api/knowledge/", knowledge)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveStatic(w, r, staticDir)
	})
	return mux, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func serveStatic(w http.ResponseWriter, r *http.Request, root string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")))
	info, err := os.Stat(name)
	asset := strings.HasPrefix(r.URL.Path, "/assets/")
	if err != nil || !info.Mode().IsRegular() {
		if asset {
			http.NotFound(w, r)
			return
		}
		name = filepath.Join(root, "index.html")
	}
	file, err := os.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if filepath.Base(name) == "index.html" {
		w.Header().Set("X-Content-Type-Options", "nosniff")
	} else if asset {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeContent(w, r, filepath.Base(name), info.ModTime(), file)
}
