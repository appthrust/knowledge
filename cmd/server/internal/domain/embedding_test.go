package domain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAICompatibleEmbedderUsesTemplatesAndLearnsDimensions(t *testing.T) {
	var inputs [][]string
	var auth []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/embeddings", r.URL.Path)
		auth = append(auth, r.Header.Get("Authorization"))
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "embeddinggemma", body.Model)
		inputs = append(inputs, body.Input)
		data := []map[string]any{}
		for i := range body.Input {
			v := make([]float32, 768)
			v[i%768] = 1
			data = append(data, map[string]any{"index": i, "embedding": v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer upstream.Close()
	t.Setenv("KNOWLEDGE_EMBEDDING_PROVIDER", "openai")
	t.Setenv("KNOWLEDGE_EMBEDDING_MODEL", "embeddinggemma")
	t.Setenv("KNOWLEDGE_EMBEDDING_BASE_URL", upstream.URL+"/v1")
	t.Setenv("KNOWLEDGE_EMBEDDING_API_KEY", "")
	t.Setenv("KNOWLEDGE_EMBEDDING_DIMENSIONS", "")
	t.Setenv("KNOWLEDGE_EMBEDDING_QUERY_TEMPLATE", "")
	t.Setenv("KNOWLEDGE_EMBEDDING_DOCUMENT_TEMPLATE", "")
	e, err := EmbeddingFromEnv()
	require.NoError(t, err)
	require.Equal(t, 0, e.Dimensions, "unknown model: learned from the first response")
	require.Equal(t, "openai:embeddinggemma", e.IndexProvider())

	chunks, err := e.Chunks(context.Background(), "有給休暇", "年次有給休暇は入社6か月後に10日付与されます。")
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Equal(t, "年次有給休暇は入社6か月後に10日付与されます。", chunks[0].Text, "stored text carries no template")
	require.Equal(t, [][]string{{"title: 有給休暇 | text: 年次有給休暇は入社6か月後に10日付与されます。"}}, inputs)
	require.Equal(t, 768, e.Dimensions)

	q, err := e.EmbedQuery(context.Background(), "有給")
	require.NoError(t, err)
	require.Len(t, q, 768)
	require.Equal(t, []string{"task: search result | query: 有給"}, inputs[1])
	require.Equal(t, []string{"", ""}, auth, "no Authorization header without a key")

	// A learned dimension is enforced afterwards.
	e.Dimensions = 512
	_, err = e.EmbedQuery(context.Background(), "x")
	require.EqualError(t, err, "invalid embedding index or dimensions")

	// api.openai.com still requires a key; other endpoints do not.
	t.Setenv("KNOWLEDGE_EMBEDDING_BASE_URL", "")
	_, err = EmbeddingFromEnv()
	require.ErrorContains(t, err, "KNOWLEDGE_EMBEDDING_API_KEY is required")
	t.Setenv("KNOWLEDGE_EMBEDDING_DOCUMENT_TEMPLATE", "no placeholder")
	t.Setenv("KNOWLEDGE_EMBEDDING_BASE_URL", upstream.URL+"/v1")
	_, err = EmbeddingFromEnv()
	require.ErrorContains(t, err, "must contain {text}")
}

func TestEmbedSplitsLongDocumentsIntoBoundedRequests(t *testing.T) {
	var sizes []int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		sizes = append(sizes, len(body.Input))
		data := []map[string]any{}
		for i := range body.Input {
			data = append(data, map[string]any{"index": i, "embedding": []float32{float32(len(sizes)), float32(i)}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer upstream.Close()
	e := &Embedder{Provider: "openai", Model: "embeddinggemma", Dimensions: 2, BaseURL: upstream.URL + "/v1", Client: upstream.Client()}
	texts := make([]string, 20)
	for i := range texts {
		texts[i] = "chunk"
	}
	vectors, err := e.Embed(context.Background(), texts)
	require.NoError(t, err)
	require.Equal(t, []int{8, 8, 4}, sizes, "a 20-chunk post takes three requests inside the built-in app's 120 s budget")
	require.Len(t, vectors, 20)
	require.Equal(t, []float32{1, 7}, vectors[7])
	require.Equal(t, []float32{2, 0}, vectors[8], "vectors keep document order across requests")
	require.Equal(t, []float32{3, 3}, vectors[19])
}

func TestEmbeddingOutageDoesNotFallBackToHashing(t *testing.T) {
	status := http.StatusOK
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if status != http.StatusOK {
			return
		}
		vector := make([]float32, 16)
		vector[0] = 1
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"data": []any{map[string]any{"index": 0, "embedding": vector}},
		}))
	}))
	defer upstream.Close()
	e := &Embedder{Provider: "openai", Source: "embedding", Model: "embeddinggemma", BaseURL: upstream.URL + "/v1", Client: upstream.Client()}

	vector, err := e.EmbedQuery(t.Context(), "before outage")
	require.NoError(t, err)
	require.Len(t, vector, 16)
	status = http.StatusServiceUnavailable
	vector, err = e.EmbedQuery(t.Context(), "during outage")
	require.EqualError(t, err, "embedding service returned HTTP 503")
	require.Nil(t, vector, "an outage must not produce a local hashing vector")
	require.Equal(t, "openai", e.Provider)
	require.Equal(t, "embedding", e.Source)
	require.Equal(t, "openai:embeddinggemma", e.IndexProvider())

	status = http.StatusOK
	vector, err = e.EmbedQuery(t.Context(), "after recovery")
	require.NoError(t, err)
	require.Len(t, vector, 16)
	upstream.Close()
	vector, err = e.EmbedQuery(t.Context(), "disconnected")
	require.EqualError(t, err, "embedding service unavailable")
	require.Nil(t, vector)
	require.Equal(t, "openai", e.Provider)
}
