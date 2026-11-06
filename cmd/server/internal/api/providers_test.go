package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestEsaConnectionStatesAndCache(t *testing.T) {
	db, options, auth, platform, bearer := fixture(t)
	status := http.StatusOK
	calls := 0
	esa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "/v1/teams", r.URL.Path)
		require.Equal(t, "Bearer esa-platform", r.Header.Get("Authorization"))
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"teams":[{"name":"docs","privacy":"closed","description":"esa docs","icon":"https://img.esa.io/docs.png"}],"next_page":null}`))
		}
	}))
	defer esa.Close()
	options.Esa = &domain.Esa{BaseURL: esa.URL, Client: esa.Client()}

	// Not platform-managed: no probe, not connected.
	h := New(db, auth, platform, options)
	w := call(t, h, "GET", "/providers/esa", "", jwt("admin"))
	require.Equal(t, 200, w.Code, w.Body.String())
	require.JSONEq(t, `{"platformManaged":false,"connected":false,"teams":[]}`, w.Body.String())
	require.Equal(t, 0, calls)

	options.EsaToken = "esa-platform"
	h = New(db, auth, platform, options)
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	h.(*API).now = func() time.Time { return now }
	for range 2 {
		w = call(t, h, "GET", "/providers/esa", "", jwt("admin"))
		require.Equal(t, 200, w.Code, w.Body.String())
		require.JSONEq(t, `{"platformManaged":true,"connected":true,"teams":[{"name":"docs","description":"esa docs","icon":"https://img.esa.io/docs.png"}]}`, w.Body.String())
	}
	require.Equal(t, 1, calls, "the probe is cached")
	require.NotContains(t, w.Body.String(), "esa-platform")

	// Expiry re-probes; a rejected token is reported, never the token itself.
	status = http.StatusUnauthorized
	now = now.Add(esaConnectionTTL + time.Second)
	w = call(t, h, "GET", "/providers/esa", "", jwt("admin"))
	require.Equal(t, 200, w.Code, w.Body.String())
	require.JSONEq(t, `{"platformManaged":true,"connected":false,"teams":[],"error":"esa returned HTTP 401"}`, w.Body.String())
	require.Equal(t, 2, calls)

	// Management-only: access keys are refused even with write scope.
	w = call(t, h, "GET", "/providers/esa", "", bearer)
	require.Equal(t, 403, w.Code)
	require.Equal(t, 2, calls)
}
