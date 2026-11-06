package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Embedder struct {
	Provider            string
	Source              string
	Dimensions          int
	Model, BaseURL, Key string
	// QueryTemplate and DocumentTemplate wrap the text sent to asymmetric
	// models (EmbeddingGemma: "task: search result | query: {text}" and
	// "title: {title} | text: {text}"). Placeholders: {text}, {title}.
	QueryTemplate, DocumentTemplate string
	Client                          *http.Client
	dimensionMu                     sync.RWMutex
}

func HTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// embedBatch bounds one embeddings request. The Embedding built-in app serves
// EmbeddingGemma on CPU at several seconds per chunk on a small WLC and
// refuses a request after 120 s; eight chunks stay well inside that budget,
// whereas 32 chunks of a long post exceeded both it and the 60 s client
// timeout (craftsman, 2026-10-15), failing every document over ~12 chunks.
const embedBatch = 8

// embeddingClient allows the built-in app's full request budget plus a margin, so
// the built-in app's own 504 is reported instead of a client-side cut.
func embeddingClient() *http.Client {
	return &http.Client{Timeout: 150 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func ProviderURL(raw, fallback string) (string, error) {
	if raw == "" {
		return fallback, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New("provider base URL must be an HTTP(S) URL without credentials, query or fragment")
	}
	return strings.TrimRight(raw, "/"), nil
}
func EmbeddingFromEnv() (*Embedder, error) {
	e := &Embedder{Provider: os.Getenv("KNOWLEDGE_EMBEDDING_PROVIDER"), Dimensions: 256, Client: embeddingClient()}
	builtinAppURL := os.Getenv("KNOWLEDGE_EMBEDDING_URL")
	baseURL := os.Getenv("KNOWLEDGE_EMBEDDING_BASE_URL")
	defaultModel := "text-embedding-3-small"
	defaultBase := "https://api.openai.com/v1"
	defaultSource := "openai"
	if e.Provider == "" {
		switch {
		case builtinAppURL != "":
			e.Provider = "openai"
			defaultBase = strings.TrimRight(builtinAppURL, "/") + "/v1"
			defaultModel = "embeddinggemma"
			defaultSource = "embedding"
		case baseURL != "" || os.Getenv("KNOWLEDGE_EMBEDDING_API_KEY") != "" || os.Getenv("KNOWLEDGE_OPENAI_API_KEY") != "":
			e.Provider = "openai"
		default:
			e.Provider = "hashing"
		}
	}
	switch e.Provider {
	case "hashing":
		e.Source = "hashing"
		return e, nil
	case "openai":
		// Any OpenAI-compatible /v1/embeddings endpoint (OpenAI, Ollama, TEI,
		// llama.cpp). The key is required only for api.openai.com.
		e.Key = os.Getenv("KNOWLEDGE_EMBEDDING_API_KEY")
		e.Model = os.Getenv("KNOWLEDGE_EMBEDDING_MODEL")
		if e.Model == "" {
			e.Model = defaultModel
		}
		var err error
		e.Source = "custom"
		if baseURL == "" {
			baseURL = defaultBase
			e.Source = defaultSource
		}
		e.BaseURL, err = ProviderURL(baseURL, "https://api.openai.com/v1")
		if err != nil {
			return nil, err
		}
		if e.BaseURL == "https://api.openai.com/v1" {
			e.Source = "openai"
		}
		if e.Key == "" && e.BaseURL == "https://api.openai.com/v1" {
			e.Key = os.Getenv("KNOWLEDGE_OPENAI_API_KEY")
		}
		if e.Key == "" && e.BaseURL == "https://api.openai.com/v1" {
			return nil, errors.New("KNOWLEDGE_EMBEDDING_API_KEY is required for api.openai.com")
		}
		switch e.Model {
		case "text-embedding-3-large":
			e.Dimensions = 3072
		case "text-embedding-3-small", "text-embedding-ada-002":
			e.Dimensions = 1536
		default:
			e.Dimensions = 0 // learned from the first response
		}
		if raw := os.Getenv("KNOWLEDGE_EMBEDDING_DIMENSIONS"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 16 || n > 16000 {
				return nil, errors.New("KNOWLEDGE_EMBEDDING_DIMENSIONS must be an integer between 16 and 16000")
			}
			e.Dimensions = n
		}
		e.QueryTemplate = os.Getenv("KNOWLEDGE_EMBEDDING_QUERY_TEMPLATE")
		e.DocumentTemplate = os.Getenv("KNOWLEDGE_EMBEDDING_DOCUMENT_TEMPLATE")
		if strings.HasPrefix(e.Model, "embeddinggemma") {
			if e.QueryTemplate == "" {
				e.QueryTemplate = "task: search result | query: {text}"
			}
			if e.DocumentTemplate == "" {
				e.DocumentTemplate = "title: {title} | text: {text}"
			}
		}
		for _, template := range []string{e.QueryTemplate, e.DocumentTemplate} {
			if template != "" && !strings.Contains(template, "{text}") {
				return nil, errors.New("KNOWLEDGE_EMBEDDING_*_TEMPLATE must contain {text}")
			}
		}
		return e, nil
	default:
		return nil, errors.New("KNOWLEDGE_EMBEDDING_PROVIDER must be hashing or openai")
	}
}

func fill(template, title, text string) string {
	if template == "" {
		return text
	}
	if title == "" {
		title = "none"
	}
	return strings.NewReplacer("{title}", title, "{text}", text).Replace(template)
}

// EmbedQuery embeds one search query with the query-side template.
func (e *Embedder) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	vectors, err := e.Embed(ctx, []string{fill(e.QueryTemplate, "", query)})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

// IndexProvider includes the model so equal-dimension, incompatible embedding
// spaces never get compared after a provider model change.
func (e *Embedder) IndexProvider() string {
	if e.Provider == "openai" {
		return e.Provider + ":" + e.Model
	}
	return e.Provider
}

// DimensionCount is safe while the background worker learns a model's dimension.
func (e *Embedder) DimensionCount() int {
	e.dimensionMu.RLock()
	defer e.dimensionMu.RUnlock()
	return e.Dimensions
}

func (e *Embedder) acceptDimensions(n int) bool {
	e.dimensionMu.Lock()
	defer e.dimensionMu.Unlock()
	if e.Dimensions == 0 && n >= 16 && n <= 16000 {
		e.Dimensions = n
	}
	return n == e.Dimensions
}
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	if e.Provider == "hashing" {
		for i, text := range texts {
			out[i] = hashEmbedding(text)
		}
		return out, nil
	}
	// Batches bound the remote request, response size and per-request time.
	for start := 0; start < len(texts); start += embedBatch {
		end := min(start+embedBatch, len(texts))
		body, _ := json.Marshal(map[string]any{"model": e.Model, "input": texts[start:end]})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if e.Key != "" {
			req.Header.Set("Authorization", "Bearer "+e.Key)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := e.Client.Do(req)
		if err != nil {
			return nil, errors.New("embedding service unavailable")
		}
		var response struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		err = json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&response)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("embedding service returned HTTP %d", res.StatusCode)
		}
		if err != nil || len(response.Data) != end-start {
			return nil, errors.New("invalid embedding response")
		}
		for _, d := range response.Data {
			if d.Index < 0 || d.Index >= end-start || out[start+d.Index] != nil || !e.acceptDimensions(len(d.Embedding)) {
				return nil, errors.New("invalid embedding index or dimensions")
			}
			norm := float64(0)
			for _, v := range d.Embedding {
				if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
					return nil, errors.New("invalid embedding value")
				}
				norm += float64(v) * float64(v)
			}
			if norm == 0 {
				return nil, errors.New("zero embedding vector")
			}
			out[start+d.Index] = d.Embedding
		}
	}
	return out, nil
}

// hashEmbedding is the egress-free fallback: feature hashing of search
// tokens and their adjacent pairs, so CJK bigrams carry the signal that
// whitespace tokenisation would lose.
func hashEmbedding(text string) []float32 {
	words := SearchTokens(text)
	v := make([]float32, 256)
	add := func(s string) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(s))
		n := h.Sum64()
		sign := float32(1)
		if n&(1<<63) != 0 {
			sign = -1
		}
		v[n%256] += sign
	}
	for i, word := range words {
		add(word)
		if i > 0 {
			add(words[i-1] + " " + word)
		}
	}
	norm := float64(0)
	for _, x := range v {
		norm += float64(x * x)
	}
	if norm == 0 {
		v[0] = 1
		return v
	}
	scale := float32(1 / math.Sqrt(norm))
	for i := range v {
		v[i] *= scale
	}
	return v
}

// Chunks splits content and embeds every chunk with the document template,
// so asymmetric models see the title next to each passage.
func (e *Embedder) Chunks(ctx context.Context, title, content string) ([]Chunk, error) {
	texts := ChunkMarkdown(content)
	inputs := make([]string, len(texts))
	for i, text := range texts {
		inputs[i] = fill(e.DocumentTemplate, title, text)
	}
	vectors, err := e.Embed(ctx, inputs)
	if err != nil {
		return nil, err
	}
	chunks := make([]Chunk, len(texts))
	for i := range texts {
		chunks[i] = Chunk{Text: texts[i], Embedding: vectors[i]}
	}
	return chunks, nil
}
