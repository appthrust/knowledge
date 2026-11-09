package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/appthrust/appserver-go/accesskey"
	"github.com/appthrust/appserver-go/apphealth"
	knowledgecontract "github.com/appthrust/appserver-go/appserver/contract/knowledge"
	"github.com/appthrust/appserver-go/appserver/health"
	"github.com/appthrust/appserver-go/appserver/identity"
	pgtest "github.com/appthrust/appserver-go/appserver/pgstore/testutil"
	"github.com/appthrust/appserver-go/appserver/platformclient"
	"github.com/appthrust/knowledge/cmd/server/internal/api"
	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/appthrust/knowledge/cmd/server/internal/store"
	"github.com/appthrust/knowledge/cmd/server/internal/testutil"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func cleanEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"KNOWLEDGE_DATABASE_URL", "DATABASE_URL", "PLATFORM_OIDC_ISSUER", "PLATFORM_OIDC_ISSUER_CA_PEM", "KNOWLEDGE_OIDC_AUDIENCES", "PLATFORM_ACCESS_ISSUER", "KNOWLEDGE_ACCESS_AUDIENCE", "KNOWLEDGE_PUBLIC_URL", "KNOWLEDGE_DEV_SEED", "KNOWLEDGE_EMBEDDING_PROVIDER"} {
		t.Setenv(key, "")
	}
	t.Setenv("KNOWLEDGE_NOTION_TOKEN", "")
	t.Setenv("KNOWLEDGE_ESA_TOKEN", "")
	for _, key := range []string{"KNOWLEDGE_OPENAI_API_KEY", "KNOWLEDGE_EMBEDDING_URL", "KNOWLEDGE_EMBEDDING_BASE_URL", "KNOWLEDGE_EMBEDDING_MODEL", "KNOWLEDGE_EMBEDDING_API_KEY", "KNOWLEDGE_EMBEDDING_DIMENSIONS", "KNOWLEDGE_EMBEDDING_QUERY_TEMPLATE", "KNOWLEDGE_EMBEDDING_DOCUMENT_TEMPLATE", "KNOWLEDGE_LLM_URL", "KNOWLEDGE_LLM_MODEL"} {
		t.Setenv(key, "")
	}
}
func TestUnavailableStoreProbesSPAAndConfig(t *testing.T) {
	cleanEnv(t)
	knowledge, cleanup, err := knowledgeHandler("http://127.0.0.1:1", "pj-test")
	require.NoError(t, err)
	defer cleanup()
	static := t.TempDir()
	h, err := newHandler(static, "http://127.0.0.1:1", "pj-test", knowledge, knowledge.Ready)
	require.NoError(t, err)
	for _, path := range []string{"/healthz", "/readyz", "/config"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, 200, w.Code)
		if path == "/config" {
			require.Contains(t, w.Body.String(), `"provider":"hashing"`)
			require.Contains(t, w.Body.String(), `"source":"hashing"`)
			require.Contains(t, w.Body.String(), `"dimensions":256`)
			require.Contains(t, w.Body.String(), `"configured":false`)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/knowledge/v1/collections", nil))
	require.Equal(t, 503, w.Code)
	require.Contains(t, w.Body.String(), "knowledge_store_unavailable")
	require.NoError(t, os.WriteFile(filepath.Join(static, "index.html"), []byte("<html>Knowledge</html>"), 0600))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/collections/manual", nil))
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "Knowledge")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil))
	require.Equal(t, 404, w.Code)
}
func TestInvalidConfigBeforeDatabase(t *testing.T) {
	cleanEnv(t)
	for _, tc := range []struct{ upstream, project string }{{"http://remote.test", "pj-test"}, {"https://valid.test", "UPPER"}, {"https://valid.test/path", "pj-test"}, {"https://valid.test", ""}} {
		_, err := newHandler(t.TempDir(), tc.upstream, tc.project, nil, nil)
		require.Error(t, err)
	}
	t.Setenv("PLATFORM_OIDC_ISSUER", "https://issuer.example.test")
	_, _, err := knowledgeHandler("https://valid.test", "pj-test")
	require.ErrorContains(t, err, "OIDC audiences are required")
}
func TestUnreachableDatabasePending(t *testing.T) {
	cleanEnv(t)
	t.Setenv("KNOWLEDGE_DATABASE_URL", "postgres://user@127.0.0.1:1/db?sslmode=disable&connect_timeout=1")
	knowledge, cleanup, err := knowledgeHandler("http://127.0.0.1:1", "pj-test")
	require.NoError(t, err)
	defer cleanup()
	h, err := newHandler(t.TempDir(), "http://127.0.0.1:1", "pj-test", knowledge, knowledge.Ready)
	require.NoError(t, err)
	for path, status := range map[string]int{"/healthz": 200, "/readyz": 503, "/api/knowledge/v1/collections": 503} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, status, w.Code)
	}
}
func TestIdentityOutageAndRelease(t *testing.T) {
	cleanEnv(t)
	issuer := pgtest.NewOIDCIssuer(t)
	issuer.Available.Store(false)
	auth, err := identity.New(identity.Trust{OIDCIssuer: issuer.Server.URL, OIDCCAPEM: issuer.CAPEM, OIDCAudiences: []string{"appthrust-platform-api"}})
	require.NoError(t, err)
	options, err := runtimeOptions("pj-test")
	require.NoError(t, err)
	for key, value := range map[string]string{"KNOWLEDGE_RELEASE_VERSION": "release", "KNOWLEDGE_SOURCE_REVISION": "", "KNOWLEDGE_ARTIFACT_DIGEST": "digest"} {
		t.Setenv(key, value)
	}
	h, err := newHandler(t.TempDir(), "http://127.0.0.1:1", "pj-test", api.New(nil, auth, nil, options), nil)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodGet, "/api/knowledge/v1/collections", nil)
	r.Header.Set("Authorization", issuer.Bearer(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	require.Equal(t, 503, w.Code)
	require.Contains(t, w.Body.String(), "identity_unavailable")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/releasez", nil))
	require.Equal(t, 200, w.Code)
	var release map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &release))
	require.Equal(t, map[string]string{"releaseVersion": "release", "artifactDigest": "digest"}, release)
}
func TestDatabaseFallbackDevSeed(t *testing.T) {
	cleanEnv(t)
	db, dsn := testutil.Database(t)
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("KNOWLEDGE_DEV_SEED", "1")
	knowledge, cleanup, err := knowledgeHandler("http://127.0.0.1:1", "pj-test")
	require.NoError(t, err)
	defer cleanup()
	require.True(t, knowledge.Ready())
	require.NoError(t, db.Do(context.Background(), false, func(tx *store.Tx) error {
		var count int
		require.NoError(t, tx.SQL.QueryRowContext(tx.Context, `SELECT count(*) FROM documents`).Scan(&count))
		require.Equal(t, 3, count)
		return nil
	}))
}
func TestVectorExtensionPrivilegeAndPending(t *testing.T) {
	cleanEnv(t)
	dsn := os.Getenv("BUILTIN_APPSERVER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BUILTIN_APPSERVER_TEST_DATABASE_URL unset")
	}
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer admin.Close()
	var super bool
	require.NoError(t, admin.QueryRow(`SELECT rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&super))
	if !super {
		t.Skip("isolated database/role test requires superuser test DSN")
	}
	name := "knowledge_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	role := name + "_owner"
	_, err = admin.Exec(`CREATE ROLE ` + pq.QuoteIdentifier(role) + ` LOGIN PASSWORD 'test' NOSUPERUSER`)
	require.NoError(t, err)
	defer func() { _, err := admin.Exec(`DROP ROLE ` + pq.QuoteIdentifier(role)); require.NoError(t, err) }()
	_, err = admin.Exec(`CREATE DATABASE ` + pq.QuoteIdentifier(name) + ` OWNER ` + pq.QuoteIdentifier(role))
	require.NoError(t, err)
	defer func() {
		_, err := admin.Exec(`DROP DATABASE ` + pq.QuoteIdentifier(name) + ` WITH (FORCE)`)
		require.NoError(t, err)
	}()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.Path = "/" + name
	superDSN := u.String()
	u.User = url.UserPassword(role, "test")
	appDSN := u.String()
	_, err = store.Open(context.Background(), appDSN)
	require.ErrorIs(t, err, store.ErrVectorPending)
	t.Setenv("DATABASE_URL", appDSN)
	knowledge, cleanup, err := knowledgeHandler("http://127.0.0.1:1", "pj-test")
	require.NoError(t, err)
	h, err := newHandler(t.TempDir(), "http://127.0.0.1:1", "pj-test", knowledge, knowledge.Ready)
	require.NoError(t, err)
	for path, status := range map[string]int{"/healthz": 200, "/readyz": 503, "/api/knowledge/v1/collections": 503} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, status, w.Code)
		if strings.Contains(path, "/api/") {
			require.Contains(t, w.Body.String(), "vector extension")
		}
	}
	cleanup()
	// The privileged dev path installs the extension and migrates its schema.
	privileged, err := store.Open(context.Background(), superDSN)
	require.NoError(t, err)
	require.NoError(t, privileged.Close())
	superDB, err := sql.Open("postgres", superDSN)
	require.NoError(t, err)
	defer superDB.Close()
	var trusted, requiresSuper bool
	require.NoError(t, superDB.QueryRow(`SELECT trusted,superuser FROM pg_available_extension_versions WHERE name='vector' AND installed`).Scan(&trusted, &requiresSuper))
	t.Logf("vector trusted=%v superuser=%v; non-superuser creation denied and pending", trusted, requiresSuper)
	// A separate app-owned schema proves migrations succeed without superuser
	// once the platform provisions vector into public.
	_, err = superDB.Exec(`CREATE SCHEMA app AUTHORIZATION ` + pq.QuoteIdentifier(role))
	require.NoError(t, err)
	q := u.Query()
	q.Set("search_path", "app,public")
	u.RawQuery = q.Encode()
	app, err := store.Open(context.Background(), u.String())
	require.NoError(t, err)
	require.NoError(t, app.Close())
}

func TestConfigPlatformManagedTokens(t *testing.T) {
	for _, env := range []string{"KNOWLEDGE_NOTION_TOKEN", "KNOWLEDGE_ESA_TOKEN"} {
		for _, token := range []string{"", "platform-secret"} {
			t.Run(fmt.Sprintf("%s managed=%t", env, token != ""), func(t *testing.T) {
				cleanEnv(t)
				t.Setenv(env, token)
				options, err := runtimeOptions("pj-test")
				require.NoError(t, err)
				require.Equal(t, token, options.NotionToken+options.EsaToken)
				require.Equal(t, "https://api.esa.io", options.Esa.BaseURL)
				h, err := newHandler(t.TempDir(), "http://127.0.0.1:1", "pj-test", nil, nil)
				require.NoError(t, err)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/config", nil))
				require.Equal(t, 200, w.Code)
				require.Contains(t, w.Body.String(), fmt.Sprintf(`"notion":{"platformManaged":%t}`, env == "KNOWLEDGE_NOTION_TOKEN" && token != ""))
				require.Contains(t, w.Body.String(), fmt.Sprintf(`"esa":{"platformManaged":%t}`, env == "KNOWLEDGE_ESA_TOKEN" && token != ""))
				require.NotContains(t, w.Body.String(), "platform-secret")
			})
		}
	}
}

func TestLauncherAuthenticationAndCurrentAppInstallation(t *testing.T) {
	cleanEnv(t)
	const bearer = "Bearer eyJhbGciOiJub25lIn0.eyJzdWIiOiJkZXYtYWRtaW4iLCJlbWFpbCI6ImFkbWluQGV4YW1wbGUudGVzdCIsIm5hbWUiOiJBZG1pbiJ9."
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != bearer {
			t.Errorf("unexpected launcher request: method=%s authorization=%q", r.Method, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/pj-test":
			_, _ = w.Write([]byte(`{"id":"pj-test","name":"開発チーム"}`))
		case "/api/v1/projects/pj-test/app-installations":
			_, _ = w.Write([]byte(`{"data":[{"id":"knowledge","definitionId":"knowledge","phase":"Ready","url":"https://knowledge.example.test","display":{"icon":"book-open"}}],"meta":{"total":1}}`))
		case "/api/v1/application-stack-definitions":
			_, _ = w.Write([]byte(`{"data":[{"id":"knowledge","title":"Knowledge","description":"Project knowledge"}],"meta":{"total":1}}`))
		default:
			t.Errorf("unexpected launcher upstream path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	handler, err := newHandler(t.TempDir(), upstream.URL, "pj-test", nil, nil)
	require.NoError(t, err)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/launcher", nil))
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Equal(t, "application/json", response.Header().Get("Content-Type"))
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	var failure map[string]string
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &failure))
	require.Equal(t, "unauthenticated", failure["error"])

	request := httptest.NewRequest(http.MethodGet, "/launcher", nil)
	request.Header.Set("Authorization", bearer)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, "application/json", response.Header().Get("Content-Type"))
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	var result struct {
		Project struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
		Entries []struct {
			DefinitionID string `json:"definitionId"`
			Current      bool   `json:"current"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.Equal(t, "pj-test", result.Project.ID)
	require.Equal(t, "開発チーム", result.Project.Name)
	require.Len(t, result.Entries, 1)
	require.Equal(t, "knowledge", result.Entries[0].DefinitionID)
	require.True(t, result.Entries[0].Current)
}

func TestLauncherRejectsInvalidIdentityCA(t *testing.T) {
	cleanEnv(t)
	t.Setenv("PLATFORM_OIDC_ISSUER", "https://issuer.example.test")
	t.Setenv("PLATFORM_OIDC_ISSUER_CA_PEM", "invalid PEM")
	handler, err := newHandler(t.TempDir(), "https://platform.example.test", "pj-test", nil, nil)
	require.ErrorContains(t, err, "PEM")
	require.Nil(t, handler)
}

func TestOpenAPIContractAuthenticatesMembersAndKeys(t *testing.T) {
	cleanEnv(t)
	committed := knowledgecontract.Document
	issuer := pgtest.NewOIDCIssuer(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	signer := accesskey.NewES256Signer("test-key", key)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/access/jwks.json", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(signer.JWKS()))
	}))
	defer jwks.Close()
	accessIssuer := accesskey.IssuerURL(jwks.URL)
	auth, err := identity.New(identity.Trust{OIDCIssuer: issuer.Server.URL, OIDCCAPEM: issuer.CAPEM, OIDCAudiences: []string{"appthrust-platform-api"}, AccessIssuer: accessIssuer, AccessAudience: "knowledge"})
	require.NoError(t, err)
	member := issuer.Bearer(t)
	assertion := func(project string) string {
		token, err := signer.Sign(accesskey.Claims{
			Issuer: accessIssuer, Audience: "knowledge", Project: project, KeyName: "test-key",
			Subject: accesskey.Subject{Kind: accesskey.SubjectComponent, ID: "component"},
			Scopes:  []string{"search"}, JTI: "test", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
		})
		require.NoError(t, err)
		return "Bearer " + token
	}
	for _, tc := range []struct {
		name, role, bearer string
		status             int
	}{
		{"admin", "Admin", member, http.StatusOK},
		{"editor", "Editor", member, http.StatusOK},
		{"viewer", "Viewer", member, http.StatusOK},
		{"key", "", assertion("pj-test"), http.StatusOK},
		{"nonmember", "", member, http.StatusForbidden},
		{"foreign key", "", assertion("pj-other"), http.StatusUnauthorized},
		{"missing bearer", "", "", http.StatusUnauthorized},
		{"invalid bearer", "", "Bearer invalid", http.StatusUnauthorized},
		{"raw key", "", "Bearer apk_knowl_invalid_checksum", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, member, r.Header.Get("Authorization"))
				switch r.URL.Path {
				case "/api/v1/projects/pj-test/access":
					_, _ = fmt.Fprintf(w, `{"member":%t,"role":%q}`, tc.role != "", tc.role)
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			knowledge := api.New(nil, auth, platformclient.New(upstream.URL, "pj-test"), api.Options{ProjectID: "pj-test"})
			h, err := newHandler(t.TempDir(), upstream.URL, "pj-test", knowledge, nil)
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodGet, "/api/knowledge/v1/openapi.json", nil)
			request.Header.Set("Authorization", tc.bearer)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			require.Equal(t, tc.status, response.Code, response.Body.String())
			require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			if tc.status == http.StatusOK {
				require.Equal(t, "application/json", response.Header().Get("Content-Type"))
				require.Equal(t, committed, response.Body.Bytes(), "served OpenAPI must exactly match the generated contract")
			} else {
				require.NotContains(t, response.Body.String(), `"openapi"`)
			}
		})
	}
}

func TestProviderEnvironmentResolution(t *testing.T) {
	for _, tc := range []struct {
		name                                                  string
		env                                                   map[string]string
		provider, source, model, base, key, llmModel, llmBase string
		dimensions                                            int
	}{
		{name: "defaults", provider: "hashing", source: "hashing", dimensions: 256},
		{name: "built-in app", env: map[string]string{"KNOWLEDGE_EMBEDDING_URL": "http://embedding.project.svc/"}, provider: "openai", source: "embedding", model: "embeddinggemma", base: "http://embedding.project.svc/v1"},
		{name: "common embedding key does not enable LLM", env: map[string]string{"KNOWLEDGE_OPENAI_API_KEY": "common"}, provider: "openai", source: "openai", model: "text-embedding-3-small", base: "https://api.openai.com/v1", key: "common", dimensions: 1536},
		{name: "embedding dependency ignores common key", env: map[string]string{"KNOWLEDGE_EMBEDDING_URL": "http://embedding.project.svc", "KNOWLEDGE_OPENAI_API_KEY": "common"}, provider: "openai", source: "embedding", model: "embeddinggemma", base: "http://embedding.project.svc/v1"},
		{name: "explicit hashing", env: map[string]string{"KNOWLEDGE_EMBEDDING_PROVIDER": "hashing", "KNOWLEDGE_EMBEDDING_URL": "http://embedding.project.svc", "KNOWLEDGE_OPENAI_API_KEY": "common"}, provider: "hashing", source: "hashing", dimensions: 256},
		{name: "explicit embedding settings and gateway model", env: map[string]string{"KNOWLEDGE_EMBEDDING_PROVIDER": "openai", "KNOWLEDGE_EMBEDDING_URL": "http://embedding.project.svc", "KNOWLEDGE_OPENAI_API_KEY": "common", "KNOWLEDGE_EMBEDDING_BASE_URL": "http://127.0.0.1:123/v1", "KNOWLEDGE_EMBEDDING_API_KEY": "embedding-key", "KNOWLEDGE_EMBEDDING_MODEL": "custom", "KNOWLEDGE_EMBEDDING_DIMENSIONS": "32", "KNOWLEDGE_LLM_URL": "http://gateway.project.svc/", "KNOWLEDGE_LLM_MODEL": "custom-chat"}, provider: "openai", source: "custom", model: "custom", base: "http://127.0.0.1:123/v1", key: "embedding-key", dimensions: 32, llmModel: "custom-chat", llmBase: "http://gateway.project.svc/v1"},
		{name: "built-in app custom model", env: map[string]string{"KNOWLEDGE_EMBEDDING_URL": "http://embedding.project.svc", "KNOWLEDGE_EMBEDDING_MODEL": "embeddinggemma:custom"}, provider: "openai", source: "embedding", model: "embeddinggemma:custom", base: "http://embedding.project.svc/v1"},
		{name: "explicit embedding endpoint without provider", env: map[string]string{"KNOWLEDGE_EMBEDDING_BASE_URL": "http://127.0.0.1:123/v1", "KNOWLEDGE_EMBEDDING_MODEL": "custom", "KNOWLEDGE_OPENAI_API_KEY": "common"}, provider: "openai", source: "custom", model: "custom", base: "http://127.0.0.1:123/v1"},
		{name: "explicit endpoint overrides built-in app", env: map[string]string{"KNOWLEDGE_EMBEDDING_URL": "http://embedding.project.svc", "KNOWLEDGE_EMBEDDING_BASE_URL": "http://other.project.svc/v1"}, provider: "openai", source: "custom", model: "embeddinggemma", base: "http://other.project.svc/v1"},
		{name: "explicit OpenAI endpoint", env: map[string]string{"KNOWLEDGE_EMBEDDING_BASE_URL": "https://api.openai.com/v1/", "KNOWLEDGE_EMBEDDING_API_KEY": "embedding-key"}, provider: "openai", source: "openai", model: "text-embedding-3-small", base: "https://api.openai.com/v1", key: "embedding-key", dimensions: 1536},
		{name: "built-in apps with gateway default model", env: map[string]string{"KNOWLEDGE_EMBEDDING_URL": "http://embedding.project.svc", "KNOWLEDGE_LLM_URL": "http://gateway.project.svc"}, provider: "openai", source: "embedding", model: "embeddinggemma", base: "http://embedding.project.svc/v1", llmBase: "http://gateway.project.svc/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanEnv(t)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			options, err := runtimeOptions("pj-test")
			require.NoError(t, err)
			e := options.Embedding
			require.Equal(t, tc.provider, e.Provider)
			require.Equal(t, tc.source, e.Source)
			require.Equal(t, tc.model, e.Model)
			require.Equal(t, tc.base, e.BaseURL)
			require.Equal(t, tc.key, e.Key)
			require.Equal(t, tc.dimensions, e.DimensionCount())
			if strings.HasPrefix(tc.model, "embeddinggemma") {
				require.Equal(t, "task: search result | query: {text}", e.QueryTemplate)
				require.Equal(t, "title: {title} | text: {text}", e.DocumentTemplate)
			}
			if tc.llmBase == "" {
				require.Nil(t, options.LLM)
			} else {
				require.NotNil(t, options.LLM)
				require.Equal(t, "gateway", options.LLM.Provider)
				require.Equal(t, tc.llmModel, options.LLM.Model)
				require.Equal(t, tc.llmBase, options.LLM.BaseURL)
			}
			h, err := newHandler(t.TempDir(), "http://127.0.0.1:1", "pj-test", nil, nil)
			require.NoError(t, err)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/config", nil))
			require.Equal(t, 200, w.Code)
			var config struct {
				Embedding domain.EmbeddingConfig `json:"embedding"`
				LLM       struct {
					Configured bool    `json:"configured"`
					Provider   *string `json:"provider"`
				} `json:"llm"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &config))
			require.Equal(t, tc.provider, config.Embedding.Provider)
			require.Equal(t, tc.source, config.Embedding.Source)
			require.Equal(t, domain.Backfill{State: "idle"}, config.Embedding.Backfill)
			require.Equal(t, tc.llmBase != "", config.LLM.Configured)
			if config.LLM.Configured {
				require.NotNil(t, config.LLM.Provider)
				require.Equal(t, "gateway", *config.LLM.Provider)
			} else {
				require.Nil(t, config.LLM.Provider)
			}
			require.NotContains(t, w.Body.String(), "common")
		})
	}
}

func TestBackfillDoesNotBlockReadinessAndConfigUsesLiveDimensions(t *testing.T) {
	cleanEnv(t)
	db, dsn := testutil.Database(t)
	hashing := &domain.Embedder{Provider: "hashing", Dimensions: 256}
	chunks, err := hashing.Chunks(context.Background(), "Original title", "Original content")
	require.NoError(t, err)
	require.NoError(t, db.Do(context.Background(), true, func(tx *store.Tx) error {
		if err := tx.PutCollection(domain.Collection{Name: "manual", Visibility: domain.Visibility{Groups: []string{}}}); err != nil {
			return err
		}
		_, err := tx.PutDocument(domain.Document{Collection: "manual", Title: "Original title", Content: "Original content"}, chunks, hashing.IndexProvider())
		return err
	}))
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		vector := make([]float32, 768)
		vector[0] = 1
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": vector}}})
	}))
	defer upstream.Close()
	t.Setenv("KNOWLEDGE_DATABASE_URL", dsn)
	t.Setenv("KNOWLEDGE_EMBEDDING_URL", upstream.URL)
	knowledge, cleanup, err := knowledgeHandler("http://127.0.0.1:1", "pj-test")
	require.NoError(t, err)
	defer cleanup()
	h, err := newHandler(t.TempDir(), "http://127.0.0.1:1", "pj-test", knowledge, knowledge.Ready)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, 200, w.Code)
	readConfig := func() domain.EmbeddingConfig {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/config", nil))
		require.Equal(t, 200, w.Code, w.Body.String())
		var config struct {
			Embedding domain.EmbeddingConfig `json:"embedding"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &config))
		return config.Embedding
	}
	before := readConfig()
	require.Equal(t, 1, before.Backfill.Pending)
	require.Contains(t, []string{"queued", "running"}, before.Backfill.State)
	require.Zero(t, before.Dimensions)
	close(release)
	require.Eventually(t, func() bool {
		after := readConfig()
		return after.Backfill.State == "idle" && after.Backfill.Pending == 0 && after.Dimensions == 768
	}, 5*time.Second, 10*time.Millisecond)
}

func TestApplicationHealthUnavailableDatabaseAndPublicBoundary(t *testing.T) {
	cleanEnv(t)
	knowledge, cleanup, err := knowledgeHandler("http://127.0.0.1:1", "pj-test")
	require.NoError(t, err)
	defer cleanup()
	h, err := newHandler(t.TempDir(), "http://127.0.0.1:1", "pj-test", knowledge, knowledge.Ready)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, health.Path, nil))
	require.Equal(t, http.StatusOK, w.Code)
	var report apphealth.Report
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &report))
	require.NoError(t, report.Validate([]string{"search", "ask", "ingest"}))
	for _, capability := range report.Capabilities {
		require.Equal(t, apphealth.StateFailing, capability.State)
		require.Equal(t, apphealth.ReasonDependencyUnavailable, capability.Reason)
	}
	for _, header := range []string{apphealth.PublicRouteHeader, "Authorization"} {
		r := httptest.NewRequest(http.MethodGet, health.Path, nil)
		r.Header.Set(header, "public")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		require.Equal(t, http.StatusNotFound, w.Code)
	}
}
