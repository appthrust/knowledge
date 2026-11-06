package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/appthrust/knowledge/cmd/server/internal/store"
	"github.com/stretchr/testify/require"
)

func TestLinkQueryValidationBeforeDatabase(t *testing.T) {
	auth, assertion := assertionIdentity(t)
	h := New(nil, auth, nil, Options{ProjectID: "pj-test"})
	bearer := assertion("pj-test", "search", "documents:read")
	for _, tc := range []struct{ path, message string }{
		{"/documents?limit=0", "limit must be between 1 and 500"},
		{"/documents?limit=-1", "limit must be between 1 and 500"},
		{"/documents?limit=501", "limit must be between 1 and 500"},
		{"/documents?limit=one", "limit must be between 1 and 500"},
		{"/documents?limit=1.5", "limit must be between 1 and 500"},
		{"/documents?limit=999999999999999999999999", "limit must be between 1 and 500"},
		{"/search", "query must contain 1 to 16384 bytes"},
		{"/search?query=%20", "query must contain 1 to 16384 bytes"},
		{"/search?query=test&topK=-1", "topK must be between 1 and 50"},
		{"/search?query=test&topK=51", "topK must be between 1 and 50"},
		{"/search?query=test&topK=one", "topK must be between 1 and 50"},
		{"/search?query=test&topK=1.5", "topK must be between 1 and 50"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response := call(t, h, http.MethodGet, tc.path, "", bearer)
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), `"error":"invalid_request"`)
			require.Contains(t, response.Body.String(), tc.message)
		})
	}
	for _, path := range []string{"/search?query=test", "/documents?limit=1"} {
		response := call(t, h, http.MethodGet, path, "", "")
		require.Equal(t, http.StatusUnauthorized, response.Code)
	}
}

func TestDocumentsLimitAfterCollectionVisibility(t *testing.T) {
	db, options, auth, platform, bearer := fixture(t)
	require.NoError(t, db.Do(context.Background(), true, func(tx *store.Tx) error {
		for _, collection := range []domain.Collection{
			{Name: "other", Visibility: domain.Visibility{Groups: []string{}}},
			{Name: "secret", Visibility: domain.Visibility{Groups: []string{"engineering"}}},
		} {
			if err := tx.PutCollection(collection); err != nil {
				return err
			}
		}
		if err := tx.Exec(`INSERT INTO documents(id,collection,title,content,updated_at) SELECT 'manual-' || lpad(n::text,3,'0'),'manual','Handbook','Content','2026-01-01'::timestamptz FROM generate_series(1,502) n`); err != nil {
			return err
		}
		return tx.Exec(`INSERT INTO documents(id,collection,title,content,updated_at) VALUES ('other','other','Other','Content','2025-01-01'),('secret','secret','Private','Content','2026-02-01')`)
	}))
	h := New(db, auth, platform, options)
	for _, tc := range []struct {
		query string
		count int
		first string
	}{
		{"", 100, "manual-001"},
		{"?limit=1", 1, "manual-001"},
		{"?limit=500", 500, "manual-001"},
		{"?collection=manual&limit=500", 500, "manual-001"},
		{"?collection=other&limit=1", 1, "other"},
		{"?collection=missing&limit=1", 0, ""},
		{"?collection=secret&limit=1", 0, ""},
	} {
		t.Run(tc.query, func(t *testing.T) {
			response := call(t, h, http.MethodGet, "/documents"+tc.query, "", bearer)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var result struct{ Data []domain.Document }
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
			require.NotNil(t, result.Data)
			require.Len(t, result.Data, tc.count)
			if tc.count > 0 {
				require.Equal(t, tc.first, result.Data[0].ID)
			}
			for _, document := range result.Data {
				require.NotEqual(t, "secret", document.Collection)
			}
		})
	}
}

func TestGETSearchMatchesPOSTAndPreservesVisibility(t *testing.T) {
	db, options, auth, platform, bearer := fixture(t)
	h := New(db, auth, platform, options)
	for _, collection := range []string{"other", "secret"} {
		groups := `[]`
		if collection == "secret" {
			groups = `["engineering"]`
		}
		response := call(t, h, http.MethodPost, "/collections", fmt.Sprintf(`{"name":%q,"visibility":{"groups":%s}}`, collection, groups), jwt("admin"))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	}
	for _, collection := range []string{"manual", "other", "secret"} {
		response := call(t, h, http.MethodPost, "/documents", fmt.Sprintf(`{"collection":%q,"title":%q,"content":"Quantum retrieval handbook"}`, collection, collection), jwt("admin"))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	}
	for _, tc := range []struct{ query, body string }{
		{"?query=Quantum", `{"query":"Quantum"}`},
		{"?query=Quantum&collections=manual,other&topK=1", `{"query":"Quantum","collections":["manual","other"],"topK":1}`},
		{"?query=Quantum&collections=secret", `{"query":"Quantum","collections":["secret"]}`},
		{"?query=Quantum&collections=missing", `{"query":"Quantum","collections":["missing"]}`},
		{"?query=Quantum&topK=0", `{"query":"Quantum","topK":0}`},
	} {
		t.Run(tc.query, func(t *testing.T) {
			get := call(t, h, http.MethodGet, "/search"+tc.query, "", bearer)
			post := call(t, h, http.MethodPost, "/search", tc.body, bearer)
			require.Equal(t, http.StatusOK, get.Code, get.Body.String())
			require.Equal(t, http.StatusOK, post.Code, post.Body.String())
			require.JSONEq(t, post.Body.String(), get.Body.String())
			require.NotContains(t, get.Body.String(), `"collection":"secret"`)
		})
	}
}
