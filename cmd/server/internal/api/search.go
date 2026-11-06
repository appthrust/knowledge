package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/appthrust/appserver-go/appserver/httpx"
	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/appthrust/knowledge/cmd/server/internal/store"
	"net/http"
)

func validateSearch(q *domain.Search) error {
	q.Query = strings.TrimSpace(q.Query)
	if q.Query == "" || len(q.Query) > 16384 {
		return httpx.Invalid("query must contain 1 to 16384 bytes")
	}
	if q.TopK == 0 {
		q.TopK = 8
	}
	if q.TopK < 1 || q.TopK > 50 {
		return httpx.Invalid("topK must be between 1 and 50")
	}
	if q.Mode == "" {
		q.Mode = "hybrid"
	}
	if q.Mode != "hybrid" && q.Mode != "lexical" && q.Mode != "vector" {
		return httpx.Invalid("mode must be hybrid, lexical or vector")
	}
	return nil
}

// retrieve runs one query through the store (lexical/vector/hybrid).
func (a *API) retrieve(s *request, q domain.Search) ([]domain.Hit, error) {
	var vector []float32
	if q.Mode != "lexical" {
		var err error
		vector, err = a.options.Embedding.EmbedQuery(s.r.Context(), q.Query)
		if err != nil {
			return nil, httpx.Fail(502, "knowledge_embedding_unavailable", "The embedding provider is unavailable")
		}
	}
	var hits []domain.Hit
	err := a.store.Do(s.r.Context(), false, func(tx *store.Tx) error {
		var err error
		hits, err = tx.Search(q, vector, a.options.Embedding.IndexProvider(), s.scope)
		return err
	})
	return hits, err
}

// retrieveExpanded fuses the original query with LLM formulations (official
// terms, rephrasings): every formulation is retrieved on its own and the
// per-document scores are summed, the original at full weight and each
// expansion at half, so a document only an expansion finds can surface but
// never outranks one the user's own words retrieved at the same rank.
func (a *API) retrieveExpanded(s *request, q domain.Search, formulations []string) ([]domain.Hit, error) {
	hits, err := a.retrieve(s, q)
	if err != nil || len(formulations) == 0 {
		return hits, err
	}
	docs := map[string]*domain.Hit{}
	order := []string{}
	for _, h := range hits {
		docs[h.DocumentID] = &h
		order = append(order, h.DocumentID)
	}
	for _, f := range formulations {
		alt := q
		alt.Query = f
		more, err := a.retrieve(s, alt)
		if err != nil {
			return nil, err
		}
		for _, h := range more {
			if existing := docs[h.DocumentID]; existing != nil {
				existing.Score += h.Score / 2
				continue
			}
			h.Score /= 2
			docs[h.DocumentID] = &h
			order = append(order, h.DocumentID)
		}
	}
	fused := make([]domain.Hit, 0, len(order))
	for _, id := range order {
		fused = append(fused, *docs[id])
	}
	sort.SliceStable(fused, func(i, j int) bool { return fused[i].Score > fused[j].Score })
	if len(fused) > q.TopK {
		fused = fused[:q.TopK]
	}
	return fused, nil
}

func llmFailure(err error, code string) error {
	var budget *domain.PromptBudgetError
	if errors.As(err, &budget) {
		return httpx.Fail(http.StatusBadRequest, "knowledge_prompt_budget_exceeded", budget.Error())
	}
	var upstream *domain.LLMError
	if errors.As(err, &upstream) && upstream.StatusCode == http.StatusForbidden {
		message := "The AI gateway denied the request"
		if upstream.Code != "" {
			message += ": " + upstream.Code
		}
		return httpx.Fail(403, "knowledge_llm_denied", message)
	}
	return httpx.Fail(502, code, "The LLM provider is unavailable")
}

func (a *API) search(s *request) (any, error) {
	var q domain.Search
	if s.r.Method == http.MethodGet {
		params := s.r.URL.Query()
		q.Query = params.Get("query")
		if collections := params.Get("collections"); collections != "" {
			q.Collections = strings.Split(collections, ",")
		}
		if raw := params.Get("topK"); raw != "" {
			var err error
			q.TopK, err = strconv.Atoi(raw)
			if err != nil {
				return nil, httpx.Invalid("topK must be between 1 and 50")
			}
		}
		q.Rerank, q.Expand = params.Get("rerank") == "true", params.Get("expand") == "true"
	} else if err := decode(s, &q); err != nil {
		return nil, err
	}
	if err := validateSearch(&q); err != nil {
		return nil, err
	}
	if (q.Rerank || q.Expand) && a.options.LLM == nil {
		return nil, httpx.Fail(503, "knowledge_llm_unavailable", "No LLM provider is configured")
	}
	topK := q.TopK
	if q.Rerank {
		// Retrieve a wider candidate set, rerank one excerpt per document,
		// then cut to the requested size.
		q.TopK = min(max(topK*3, 30), 50)
	}
	var formulations []string
	if q.Expand {
		var err error
		if formulations, err = a.options.LLM.Expand(s.r.Context(), s.r.Header.Get("Authorization"), q.Query); err != nil {
			return nil, llmFailure(err, "knowledge_llm_unavailable")
		}
	}
	hits, err := a.retrieveExpanded(s, q, formulations)
	if err != nil {
		return nil, err
	}
	if q.Rerank {
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.DocumentID)
		}
		var intros map[string]string
		if err := a.store.Do(s.r.Context(), false, func(tx *store.Tx) error {
			var err error
			intros, err = tx.Intros(ids, 500)
			return err
		}); err != nil {
			return nil, err
		}
		if hits, err = a.options.LLM.Rerank(s.r.Context(), s.r.Header.Get("Authorization"), q.Query, hits, intros); err != nil {
			return nil, llmFailure(err, "knowledge_llm_unavailable")
		}
		if len(hits) > topK {
			hits = hits[:topK]
		}
	}
	if formulations == nil {
		formulations = []string{}
	}
	return map[string]any{"hits": hits, "mode": q.Mode, "reranked": q.Rerank, "expanded": formulations, "embedding": map[string]string{"provider": a.options.Embedding.Provider, "source": a.options.Embedding.Source}}, nil
}

type streamed struct{ failed bool }

func (a *API) answer(s *request) (any, error) {
	if a.options.LLM == nil {
		return nil, httpx.Fail(503, "knowledge_llm_unavailable", "No LLM provider is configured")
	}
	var input struct {
		Query       string   `json:"query"`
		Collections []string `json:"collections"`
	}
	if err := decode(s, &input); err != nil {
		return nil, err
	}
	q := domain.Search{Query: input.Query, Collections: input.Collections}
	if err := validateSearch(&q); err != nil {
		return nil, err
	}
	hits, err := a.retrieve(s, q)
	if err != nil {
		return nil, err
	}
	response, err := a.options.LLM.Start(s.r.Context(), s.r.Header.Get("Authorization"), q.Query, hits)
	if err != nil {
		return nil, llmFailure(err, "knowledge_llm_error")
	}
	defer response.Body.Close()
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("X-Accel-Buffering", "no")
	emit := func(event string, payload any) error {
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, body); err != nil {
			return err
		}
		return http.NewResponseController(s.w).Flush()
	}
	for _, hit := range hits {
		if err := emit("citation", hit); err != nil {
			return streamed{failed: true}, nil
		}
	}
	err = domain.StreamTokens(response, func(text string) error { return emit("token", map[string]string{"text": text}) })
	if err != nil {
		_ = emit("error", map[string]string{"error": "knowledge_llm_error", "message": "The LLM stream did not complete"})
		return streamed{failed: true}, nil
	}
	_ = emit("done", struct{}{})
	return streamed{}, nil
}
