package domain

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestChunkMarkdownAndHashing(t *testing.T) {
	content := "# Heading\n\n" + strings.Repeat("Markdown knowledge 検索。 ", 500) + "\n\n```go\nfmt.Println(\"ok\")\n```"
	chunks := ChunkMarkdown(content)
	require.Greater(t, len(chunks), 1)
	require.Equal(t, chunks, ChunkMarkdown(content))
	for _, c := range chunks {
		require.True(t, utf8.ValidString(c))
		require.LessOrEqual(t, len([]rune(c)), 3200)
	}
	for i := 1; i < len(chunks); i++ {
		require.Contains(t, chunks[i-1], string([]rune(chunks[i])[:100]))
	}
	t.Setenv("KNOWLEDGE_EMBEDDING_PROVIDER", "")
	e, err := EmbeddingFromEnv()
	require.NoError(t, err)
	vectors, err := e.Embed(context.Background(), []string{"Knowledge SEARCH", "knowledge search", ""})
	require.NoError(t, err)
	require.Equal(t, vectors[0], vectors[1])
	require.Len(t, vectors[0], 256)
	for _, v := range vectors {
		norm := 0.0
		for _, n := range v {
			norm += float64(n * n)
		}
		require.InDelta(t, 1, math.Sqrt(norm), 1e-6)
	}
}
func TestOpenAIEmbedding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/embeddings", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		var input struct {
			Model string
			Input []string
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		require.Equal(t, "text-embedding-3-small", input.Model)
		v := make([]float32, 1536)
		v[0] = 1
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": v}}})
	}))
	defer upstream.Close()
	t.Setenv("KNOWLEDGE_EMBEDDING_PROVIDER", "openai")
	t.Setenv("KNOWLEDGE_EMBEDDING_API_KEY", "test-key")
	t.Setenv("KNOWLEDGE_EMBEDDING_BASE_URL", upstream.URL+"/v1")
	t.Setenv("KNOWLEDGE_EMBEDDING_MODEL", "")
	e, err := EmbeddingFromEnv()
	require.NoError(t, err)
	vectors, err := e.Embed(context.Background(), []string{"hello"})
	require.NoError(t, err)
	require.Len(t, vectors[0], 1536)
	require.Equal(t, "openai:text-embedding-3-small", e.IndexProvider())
}
func TestNotionSearchNestedBlocksPaginationAndIncremental(t *testing.T) {
	edited := "2026-10-14T00:00:00Z"
	blockCalls := 0
	searchCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "2022-06-28", r.Header.Get("Notion-Version"))
		require.Equal(t, "Bearer notion-secret", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/search":
			searchCalls++
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if body["start_cursor"] == "next" {
				_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"object": "page", "id": "page-1", "last_edited_time": edited, "url": "https://notion.test/page-1", "properties": map[string]any{"Name": map[string]any{"title": []any{map[string]any{"plain_text": "Handbook"}}}}}}, "has_more": true, "next_cursor": "next"})
		case "/v1/blocks/page-1/children":
			blockCalls++
			if r.URL.Query().Get("start_cursor") == "blocks-next" {
				_, _ = w.Write([]byte(`{"results":[{"id":"last","type":"paragraph","paragraph":{"rich_text":[{"plain_text":"Pagination footer"}]}}],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"nested","type":"heading_1","has_children":true,"heading_1":{"rich_text":[{"plain_text":"Nested handbook"}]}}],"has_more":true,"next_cursor":"blocks-next"}`))
		case "/v1/blocks/nested/children":
			blockCalls++
			_, _ = w.Write([]byte(`{"results":[{"id":"code","type":"code","code":{"language":"go","rich_text":[{"text":{"content":"fmt.Println(42)"}}]}}],"has_more":false}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	n := &Notion{BaseURL: upstream.URL, Client: upstream.Client()}
	revisions := map[string]string{}
	documents := []Document{}
	run := func() {
		errs, err := n.Sync(context.Background(), SourceConfig{Token: "notion-secret"}, func(id, edited string) (bool, error) { return revisions[id] == edited, nil }, func(d Document, rev string) error {
			documents = append(documents, d)
			revisions[d.ExternalID] = rev
			return nil
		})
		require.NoError(t, err)
		require.Empty(t, errs)
	}
	run()
	require.Len(t, documents, 1)
	require.Contains(t, documents[0].Content, "# Nested handbook")
	require.Contains(t, documents[0].Content, "```go\nfmt.Println(42)\n```")
	require.Contains(t, documents[0].Content, "Pagination footer")
	require.Equal(t, 3, blockCalls)
	run()
	require.Len(t, documents, 1)
	require.Equal(t, 3, blockCalls)
	require.Equal(t, 4, searchCalls)
	edited = "2026-10-15T00:00:00Z"
	run()
	require.Len(t, documents, 2)
}
func TestNotionExplicitRootsAndDatabases(t *testing.T) {
	paths := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/pages/root":
			_, _ = w.Write([]byte(`{"id":"root","object":"page","last_edited_time":"1"}`))
		case "/v1/databases/database/query":
			_, _ = w.Write([]byte(`{"results":[{"id":"row","object":"page","last_edited_time":"1"}],"has_more":false}`))
		default:
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		}
	}))
	defer upstream.Close()
	n := &Notion{BaseURL: upstream.URL, Client: upstream.Client()}
	count := 0
	errs, err := n.Sync(context.Background(), SourceConfig{Token: "secret", RootPageIDs: []string{"root"}, DatabaseIDs: []string{"database"}}, func(string, string) (bool, error) { return false, nil }, func(Document, string) error { count++; return nil })
	require.NoError(t, err)
	require.Empty(t, errs)
	require.Equal(t, 2, count)
	require.Contains(t, paths, "GET /v1/pages/root")
	require.Contains(t, paths, "POST /v1/databases/database/query")
	require.NotContains(t, paths, "POST /v1/search")
}

func TestNotionUnchangedRootStillDiscoversEditedChild(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/pages/root":
			_, _ = w.Write([]byte(`{"id":"root","object":"page","last_edited_time":"same"}`))
		case "/v1/pages/child":
			_, _ = w.Write([]byte(`{"id":"child","object":"page","last_edited_time":"new"}`))
		case "/v1/blocks/root/children":
			_, _ = w.Write([]byte(`{"results":[{"id":"child","type":"child_page","child_page":{"title":"Child"}}],"has_more":false}`))
		case "/v1/blocks/child/children":
			_, _ = w.Write([]byte(`{"results":[{"id":"text","type":"paragraph","paragraph":{"rich_text":[{"plain_text":"Edited descendant"}]}}],"has_more":false}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	n := &Notion{BaseURL: upstream.URL, Client: upstream.Client()}
	docs := []Document{}
	errs, err := n.Sync(context.Background(), SourceConfig{Token: "secret", RootPageIDs: []string{"root"}},
		func(id, edited string) (bool, error) { return id == "root" && edited == "same", nil },
		func(d Document, _ string) error { docs = append(docs, d); return nil })
	require.NoError(t, err)
	require.Empty(t, errs)
	require.Len(t, docs, 1)
	require.Equal(t, "child", docs[0].ExternalID)
	require.Equal(t, "Edited descendant", docs[0].Content)
}
