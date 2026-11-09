package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/appthrust/appserver-go/accesskey"
	"github.com/appthrust/appserver-go/appserver/identity"
	"github.com/appthrust/appserver-go/appserver/platformclient"
	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/stretchr/testify/require"
)

func assertionIdentity(t *testing.T) (*identity.Authenticator, func(string, ...string) string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	signer := accesskey.NewES256Signer("test-key", key)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/access/jwks.json", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(signer.JWKS()))
	}))
	t.Cleanup(server.Close)
	issuer := accesskey.IssuerURL(server.URL)
	auth, err := identity.New(identity.Trust{AccessIssuer: issuer, AccessAudience: "knowledge"})
	require.NoError(t, err)
	return auth, func(project string, scopes ...string) string {
		token, err := signer.Sign(accesskey.Claims{
			Issuer: issuer, Audience: "knowledge", Project: project, KeyName: "test-key",
			Subject: accesskey.Subject{Kind: accesskey.SubjectComponent, ID: "component"},
			Scopes:  scopes, JTI: "test", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
		})
		require.NoError(t, err)
		return "Bearer " + token
	}
}

func TestKeyAuthenticationAndRouteScopes(t *testing.T) {
	auth, assertion := assertionIdentity(t)
	search := assertion("pj-test", "search")
	read := assertion("pj-test", "documents:read")
	write := assertion("pj-test", "documents:write")
	secret, err := accesskey.Generate("knowl", rand.Reader)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, method, path, bearer, body string
		status                           int
		code                             string
	}{
		{"search", "POST", "/search", search, `{}`, 400, "invalid_request"},
		{"answer", "POST", "/answer", search, `{"query":"test"}`, 503, "knowledge_llm_unavailable"},
		{"create denied", "POST", "/documents", search, `{}`, 403, "insufficient_scope"},
		{"delete denied", "DELETE", "/documents/doc", read, "", 403, "insufficient_scope"},
		{"read denied", "GET", "/documents", search, "", 403, "insufficient_scope"},
		{"write", "POST", "/documents", write, `{}`, 400, "invalid_request"},
		{"read", "GET", "/documents?limit=0", read, "", 400, "invalid_request"},
		{"jobs denied", "GET", "/jobs/job", read, "", 403, "insufficient_scope"},
		{"sources", "GET", "/sources", write, "", 403, "key_not_permitted"},
		{"collection create needs write", "POST", "/collections", read, `{}`, 403, "insufficient_scope"},
		{"collection create", "POST", "/collections", write, `{}`, 400, "invalid_request"},
		{"collection delete", "DELETE", "/collections/manual", write, "", 403, "key_not_permitted"},
		{"foreign project", "POST", "/search", assertion("pj-other", "search"), `{}`, 401, "unauthenticated"},
		{"no scopes", "GET", "/openapi.json", assertion("pj-test"), "", 403, "insufficient_scope"},
		{"raw key", "POST", "/search", "Bearer " + secret.String(), `{}`, 401, "raw_key_not_accepted"},
		{"missing bearer", "POST", "/search", "", `{}`, 401, "unauthenticated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(nil, auth, nil, Options{ProjectID: "pj-test"})
			response := call(t, h, tc.method, tc.path, tc.body, tc.bearer)
			require.Equal(t, tc.status, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), `"error":"`+tc.code+`"`)
		})
	}
	// Exercise successful authentication independently of database availability.
	a := &API{identity: auth, options: Options{ProjectID: "pj-test"}}
	for _, route := range []struct {
		method, path, bearer string
		mutation             bool
	}{
		{"POST", "/search", search, false},
		{"DELETE", "/documents/doc", write, true},
		{"GET", "/jobs/job", write, false},
	} {
		r := httptest.NewRequest(route.method, "/api/knowledge/v1"+route.path, nil)
		r.Header.Set("Authorization", route.bearer)
		scope, err := a.authenticate(r, false, route.mutation)
		require.NoError(t, err)
		require.False(t, scope.Admin)
		require.Empty(t, scope.Groups)
		require.False(t, scope.Visible(domain.Collection{Visibility: domain.Visibility{Groups: []string{"engineering"}}}))
	}
}
func jwt(subject string) string {
	return "Bearer eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"`+subject+`","email":"user@example.test","groups":["engineering"]}`)) + "."
}
func TestOIDCRolesAndGroups(t *testing.T) {
	for _, role := range []string{"Admin", "Editor", "Viewer"} {
		t.Run(role, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/projects/pj-test/access":
					_, _ = w.Write([]byte(`{"member":true,"role":"` + role + `"}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			auth, err := identity.New(identity.Trust{})
			require.NoError(t, err)
			a := &API{identity: auth, platform: platformclient.New(upstream.URL, "pj-test")}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Authorization", jwt(role))
			scope, err := a.authenticate(r, false, false)
			require.NoError(t, err)
			require.Equal(t, role == "Admin", scope.Admin)
			require.Equal(t, []string{"engineering"}, scope.Groups)
			_, err = a.authenticate(r, true, false)
			if role == "Viewer" {
				require.ErrorContains(t, err, "forbidden")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
func TestAnswerUnavailableWithoutDatabase(t *testing.T) {
	auth, assertion := assertionIdentity(t)
	h := New(nil, auth, nil, Options{ProjectID: "pj-test"})
	r := httptest.NewRequest(http.MethodPost, "/api/knowledge/v1/answer", strings.NewReader(`{"query":"test"}`))
	r.Header.Set("Authorization", assertion("pj-test", "search"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	require.Equal(t, 503, w.Code)
	require.Contains(t, w.Body.String(), `"error":"knowledge_llm_unavailable"`)
}

func TestSearchOnlyKeyCanSearchButCannotWriteDocuments(t *testing.T) {
	db, options, _, platform, _ := fixture(t)
	auth, assertion := assertionIdentity(t)
	h := New(db, auth, platform, options)
	bearer := assertion("pj-test", "search")
	w := call(t, h, "POST", "/search", `{"query":"test"}`, bearer)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		path := "/documents"
		if method == http.MethodDelete {
			path += "/document"
		}
		w = call(t, h, method, path, `{}`, bearer)
		require.Equal(t, http.StatusForbidden, w.Code)
		require.Contains(t, w.Body.String(), `"error":"insufficient_scope"`)
	}
}
