package domain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEsaQueryExcludesDraftsUnlessOptedIn(t *testing.T) {
	require.Equal(t, "wip:false -in:Archived", EsaQuery(SourceConfig{}))
	require.Equal(t, "in:Engineering wip:false -in:Archived", EsaQuery(SourceConfig{Query: " in:Engineering "}))
	require.Equal(t, "-in:Archived", EsaQuery(SourceConfig{IncludeWIP: true}))
	require.Equal(t, "tag:api", EsaQuery(SourceConfig{Query: "tag:api", IncludeWIP: true, IncludeArchived: true}))
	for team, valid := range map[string]bool{"docs": true, "my-team1": true, "": false, "Docs": false, "-x": false, "a/b": false, strings.Repeat("a", 64): false} {
		require.Equal(t, valid, ValidEsaTeam(team), team)
	}
}

func TestEsaSyncPaginatesIncrementallyAndContinuesPastBadPosts(t *testing.T) {
	updated := "2026-10-15T10:00:00+09:00"
	requests := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer esa-secret", r.Header.Get("Authorization"))
		require.Equal(t, "/v1/teams/docs/posts", r.URL.Path)
		q := r.URL.Query()
		require.Equal(t, "in:Engineering wip:false -in:Archived", q.Get("q"))
		require.Equal(t, "100", q.Get("per_page"))
		require.Equal(t, "number", q.Get("sort"))
		requests = append(requests, q.Get("page"))
		w.Header().Set("Content-Type", "application/json")
		switch q.Get("page") {
		case "1":
			_ = json.NewEncoder(w).Encode(map[string]any{"posts": []any{
				map[string]any{"number": 1, "name": "Getting started", "full_name": "Engineering/Getting started #api", "category": "Engineering", "tags": []string{"api"}, "wip": false, "body_md": "# Getting started\n\nesa handbook", "url": "https://docs.esa.io/posts/1", "updated_at": updated, "revision_number": 3},
				map[string]any{"number": 0, "name": "broken", "body_md": "x", "updated_at": updated},
			}, "next_page": 2, "page": 1})
		case "2":
			_ = json.NewEncoder(w).Encode(map[string]any{"posts": []any{
				map[string]any{"number": 7, "name": "", "full_name": "Untitled #draft", "body_md": strings.Repeat("x", MaxContentBytes+1), "url": "https://docs.esa.io/posts/7", "updated_at": updated},
				map[string]any{"number": 9, "name": "Runbook", "body_md": "Runbook body", "url": "https://docs.esa.io/posts/9", "updated_at": updated},
			}, "next_page": nil, "page": 2})
		default:
			t.Errorf("unexpected page %s", q.Get("page"))
		}
	}))
	defer upstream.Close()
	e := &Esa{BaseURL: upstream.URL, Client: upstream.Client()}
	revisions := map[string]string{}
	docs := []Document{}
	run := func() []string {
		errs, err := e.Sync(context.Background(), SourceConfig{Token: "esa-secret", Team: "docs", Query: "in:Engineering"},
			func(id, edited string) (bool, error) { return revisions[id] == edited, nil },
			func(d Document, rev string) error { docs = append(docs, d); revisions[d.ExternalID] = rev; return nil })
		require.NoError(t, err)
		return errs
	}
	errs := run()
	require.Equal(t, []string{"post 0: invalid esa post number", "post 7: esa post exceeds content size limit"}, errs)
	require.Equal(t, []string{"1", "2"}, requests)
	require.Len(t, docs, 2)
	require.Equal(t, Document{ExternalID: "1", Title: "Engineering/Getting started #api", URL: "https://docs.esa.io/posts/1", Content: "# Getting started\n\nesa handbook", Metadata: map[string]any{"number": 1, "wip": false, "updatedAt": updated, "watchers": 0, "stars": 0, "comments": 0, "category": "Engineering", "tags": []string{"api"}}}, docs[0])
	require.Equal(t, "Runbook", docs[1].Title)
	run()
	require.Len(t, docs, 2, "unchanged posts are not re-emitted")
	updated = "2026-10-16T00:00:00+09:00"
	run()
	require.Len(t, docs, 4)
}

func TestEsaSyncReportsRateLimitAndInvalidTeam(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Reset", "1728609300")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	e := &Esa{BaseURL: upstream.URL, Client: upstream.Client()}
	noop := func(Document, string) error { return nil }
	fresh := func(string, string) (bool, error) { return false, nil }
	errs, err := e.Sync(context.Background(), SourceConfig{Token: "t", Team: "docs"}, fresh, noop)
	require.Empty(t, errs)
	require.EqualError(t, err, "esa rate limit exceeded (resets at unix 1728609300)")
	errs, err = e.Sync(context.Background(), SourceConfig{Token: "t", Team: "../admin"}, fresh, noop)
	require.Empty(t, errs)
	require.EqualError(t, err, "invalid esa team")
}
