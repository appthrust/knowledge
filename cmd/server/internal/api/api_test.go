package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/appthrust/appserver-go/appserver/httpx"
	"github.com/appthrust/appserver-go/appserver/identity"
	"github.com/appthrust/appserver-go/appserver/platformclient"
	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/appthrust/knowledge/cmd/server/internal/store"
	"github.com/appthrust/knowledge/cmd/server/internal/testutil"
	"github.com/stretchr/testify/require"
)

func fixture(t *testing.T) (*store.Store, Options, *identity.Authenticator, *platformclient.Client, string) {
	t.Helper()
	db, dsn := testutil.Database(t)
	dsns[db] = dsn
	e := &domain.Embedder{Provider: "hashing", Source: "hashing", Dimensions: 256}
	require.NoError(t, db.Do(context.Background(), true, func(tx *store.Tx) error {
		return tx.PutCollection(domain.Collection{Name: "manual", Visibility: domain.Visibility{Groups: []string{}}})
	}))
	auth, assertion := assertionIdentity(t)
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects/pj-test/access", r.URL.Path)
		_, _ = w.Write([]byte(`{"member":true,"role":"Admin"}`))
	}))
	t.Cleanup(platform.Close)
	return db, Options{ProjectID: "pj-test", Embedding: e}, auth, platformclient.New(platform.URL, "pj-test"), assertion("pj-test", "search", "documents:read", "documents:write")
}

// dsns lets a test reopen the same database to exercise startup paths.
var dsns = map[*store.Store]string{}

func call(t *testing.T, h http.Handler, method, path, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "/api/knowledge/v1"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", bearer)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRouteDeadlines(t *testing.T) {
	auth, err := identity.New(identity.Trust{})
	require.NoError(t, err)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects/pj-test/access", r.URL.Path)
		_, _ = w.Write([]byte(`{"member":true,"role":"Admin"}`))
	}))
	defer upstream.Close()
	platform := platformclient.New(upstream.URL, "pj-test")
	for _, tc := range []struct {
		method, path string
		want         time.Duration
	}{
		{"POST", "/answer", 330 * time.Second},
		{"POST", "/search", 2 * time.Minute},
		{"GET", "/collections", 2 * time.Minute},
		{"GET", "/answer", 2 * time.Minute},
		{"POST", "/answer/other", 2 * time.Minute},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			for _, parentBound := range []bool{false, true} {
				parent, want := context.Background(), tc.want
				if parentBound {
					bounded, cancel := context.WithTimeout(context.Background(), time.Minute)
					defer cancel()
					parent, want = bounded, time.Minute
				}
				a := &API{identity: auth, platform: platform, mux: http.NewServeMux()}
				var released <-chan struct{}
				started := time.Now()
				a.route(tc.method+" /api/knowledge/v1"+tc.path, false, false, func(s *request) (any, error) {
					released = s.r.Context().Done()
					deadline, ok := s.r.Context().Deadline()
					require.True(t, ok)
					require.WithinDuration(t, started.Add(want), deadline, time.Second)
					return map[string]bool{"ok": true}, nil
				})
				r := httptest.NewRequest(tc.method, "/api/knowledge/v1"+tc.path, nil).WithContext(parent)
				r.Header.Set("Authorization", jwt("admin"))
				w := httptest.NewRecorder()
				a.ServeHTTP(w, r)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				require.NotNil(t, released)
				select {
				case <-released:
				default:
					t.Fatal("request context must be released after the handler returns")
				}
			}
		})
	}
}
func TestDocumentUpsertSearchModesAndVisibility(t *testing.T) {
	db, options, auth, platform, bearer := fixture(t)
	h := New(db, auth, platform, options)
	post := func(content string) string {
		w := call(t, h, "POST", "/documents", `{"collection":"manual","externalId":"handbook","title":"Handbook","content":"`+content+`"}`, bearer)
		require.Equal(t, 200, w.Code, w.Body.String())
		var d domain.Document
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &d))
		require.Equal(t, 1, d.Chunks)
		return d.ID
	}
	id := post("Old banana handbook")
	require.Equal(t, id, post("Quantum retrieval handbook for developers"))
	for _, mode := range []string{"hybrid", "vector", "lexical"} {
		w := call(t, h, "POST", "/search", `{"query":"Quantum retrieval","mode":"`+mode+`"}`, bearer)
		require.Equal(t, 200, w.Code, w.Body.String())
		var result struct {
			Hits      []domain.Hit
			Mode      string
			Embedding domain.EmbeddingConfig
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		require.NotEmpty(t, result.Hits)
		require.Equal(t, id, result.Hits[0].DocumentID)
		require.Equal(t, mode, result.Mode)
		require.Equal(t, "hashing", result.Embedding.Provider)
		require.Equal(t, "hashing", result.Embedding.Source)
		require.Contains(t, result.Hits[0].Text, "Quantum")
	}
	w := call(t, h, "GET", "/documents?collection=manual", "", bearer)
	require.Equal(t, 200, w.Code)
	var listed struct{ Data []domain.Document }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listed))
	require.Len(t, listed.Data, 1)
	w = call(t, h, "GET", "/documents/"+id, "", bearer)
	require.Equal(t, 200, w.Code)
	w = call(t, h, "POST", "/collections", `{"name":"pushed"}`, bearer)
	require.Equal(t, 200, w.Code, "documents:write keys create group-less collections: "+w.Body.String())
	w = call(t, h, "POST", "/documents", `{"collection":"pushed","title":"Pushed","content":"Quantum pushed"}`, bearer)
	require.Equal(t, 200, w.Code, w.Body.String())
	w = call(t, h, "POST", "/collections", `{"name":"restricted","visibility":{"groups":["engineering"]}}`, bearer)
	require.Equal(t, 403, w.Code, "keys cannot create group-restricted collections")
	w = call(t, h, "DELETE", "/collections/pushed", "", bearer)
	require.Equal(t, 403, w.Code, "collection deletion stays a management operation")
	w = call(t, h, "POST", "/collections", `{"name":"secret","visibility":{"groups":["engineering"]}}`, jwt("admin"))
	require.Equal(t, 200, w.Code, w.Body.String())
	w = call(t, h, "POST", "/documents", `{"collection":"secret","title":"Private","content":"Quantum private"}`, bearer)
	require.Equal(t, 404, w.Code)
	w = call(t, h, "POST", "/documents", `{"collection":"secret","title":"Private","content":"Quantum private"}`, jwt("admin"))
	require.Equal(t, 200, w.Code, w.Body.String())
	w = call(t, h, "POST", "/search", `{"query":"Quantum","collections":["secret"]}`, bearer)
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"hits":[]`)
	w = call(t, h, "DELETE", "/documents/"+id, "", bearer)
	require.Equal(t, 200, w.Code)
	w = call(t, h, "POST", "/search", `{"query":"Quantum","collections":["manual"]}`, bearer)
	require.Contains(t, w.Body.String(), `"hits":[]`)
	w = call(t, h, "POST", "/search", `{"query":"x","topK":51}`, bearer)
	require.Equal(t, 400, w.Code)
}

func TestSearchReportsBuiltInEmbeddingSource(t *testing.T) {
	db, options, auth, platform, bearer := fixture(t)
	for _, key := range []string{"KNOWLEDGE_EMBEDDING_PROVIDER", "KNOWLEDGE_EMBEDDING_BASE_URL", "KNOWLEDGE_EMBEDDING_MODEL", "KNOWLEDGE_EMBEDDING_API_KEY", "KNOWLEDGE_OPENAI_API_KEY", "KNOWLEDGE_EMBEDDING_DIMENSIONS", "KNOWLEDGE_EMBEDDING_QUERY_TEMPLATE", "KNOWLEDGE_EMBEDDING_DOCUMENT_TEMPLATE"} {
		t.Setenv(key, "")
	}
	t.Setenv("KNOWLEDGE_EMBEDDING_URL", "http://embedding.project.svc")
	var err error
	options.Embedding, err = domain.EmbeddingFromEnv()
	require.NoError(t, err)
	h := New(db, auth, platform, options)
	w := call(t, h, "POST", "/search", `{"query":"handbook","mode":"lexical"}`, bearer)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var result struct {
		Embedding domain.EmbeddingConfig
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.Equal(t, "openai", result.Embedding.Provider)
	require.Equal(t, "embedding", result.Embedding.Source)
}

func TestSearchMatchesKeywordsInsideJapaneseProse(t *testing.T) {
	db, options, auth, platform, bearer := fixture(t)
	h := New(db, auth, platform, options)
	for _, doc := range []string{
		`{"collection":"manual","externalId":"ceatec","title":"展示会","content":"今年もCEATECに出展します。ブースはHall 5、デモは組み込みアプリのKnowledgeです。"}`,
		`{"collection":"manual","externalId":"expense","title":"経費精算","content":"経費精算の締め切りは月末です。領収書を添付してください。"}`,
		`{"collection":"manual","externalId":"hpa","title":"Scaling","content":"KubernetesのHPA設定はCPU使用率で決まります。"}`,
	} {
		w := call(t, h, "POST", "/documents", doc, bearer)
		require.Equal(t, 200, w.Code, w.Body.String())
	}
	search := func(query, mode string) []domain.Hit {
		w := call(t, h, "POST", "/search", `{"query":"`+query+`","mode":"`+mode+`"}`, bearer)
		require.Equal(t, 200, w.Code, w.Body.String())
		var result struct{ Hits []domain.Hit }
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		return result.Hits
	}
	// Before Go-side tokenisation "今年もCEATECに出展します" was one Postgres token
	// and "ceatec" matched nothing lexically.
	for _, mode := range []string{"lexical", "hybrid", "vector"} {
		hits := search("ceatec", mode)
		require.NotEmpty(t, hits, mode)
		require.Equal(t, "展示会", hits[0].Title, mode)
	}
	require.Equal(t, "展示会", search("ＣＥＡＴＥＣ", "lexical")[0].Title, "full-width query normalises to ASCII")
	require.Equal(t, "経費精算", search("領収書", "lexical")[0].Title, "a Japanese word is a conjunction of its bigrams")
	require.Equal(t, "Scaling", search("HPA", "lexical")[0].Title)
	require.Empty(t, search("月末", "lexical")[1:], "bigram conjunction does not match unrelated documents")

	// Rows that predate migration 003 carry no search_text; opening the store
	// tokenises them so existing indexes become searchable without a re-sync.
	require.NoError(t, db.Do(context.Background(), true, func(tx *store.Tx) error {
		return tx.Exec(`UPDATE chunks SET search_text=''`)
	}))
	require.Empty(t, search("ceatec", "lexical"))
	reopened, err := store.Open(context.Background(), dsns[db])
	require.NoError(t, err)
	defer reopened.Close()
	h = New(reopened, auth, platform, options)
	require.Equal(t, "展示会", search("ceatec", "lexical")[0].Title)
}

func TestSearchFusesPerDocumentWithPhraseLexicalAndReranks(t *testing.T) {
	db, options, auth, platform, bearer := fixture(t)
	h := New(db, auth, platform, options)
	long := strings.Repeat("有給休暇の申請は前日までにSlackとKOTで行います。", 120) // several chunks, all about 有給
	for _, doc := range []string{
		`{"collection":"manual","externalId":"130","title":"従業員用ガイド/勤怠/有給休暇の申請方法","content":"` + long + `","metadata":{"watchers":9,"stars":3}}`,
		`{"collection":"manual","externalId":"384","title":"従業員用ガイド/勤怠/年次有給休暇取得について","content":"年次有給休暇は年5日の取得義務があります。","metadata":{"watchers":1}}`,
		`{"collection":"manual","externalId":"131","title":"経費精算","content":"経費精算の締め切りは月末です。領収書を添付してください。"}`,
		`{"collection":"manual","externalId":"141","title":"社内Wi-Fi","content":"来客にはguestネットワークを案内してください。"}`,
	} {
		w := call(t, h, "POST", "/documents", doc, bearer)
		require.Equal(t, 200, w.Code, w.Body.String())
	}
	search := func(body string) []domain.Hit {
		w := call(t, h, "POST", "/search", body, bearer)
		require.Equal(t, 200, w.Code, w.Body.String())
		var result struct{ Hits []domain.Hit }
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		return result.Hits
	}
	ids := func(hits []domain.Hit) []string {
		out := []string{}
		for _, h := range hits {
			out = append(out, h.Title[strings.LastIndex(h.Title, "/")+1:])
		}
		return out
	}

	// Per-document fusion: the long document appears once, with its metadata.
	hits := search(`{"query":"有給","mode":"hybrid"}`)
	require.Equal(t, []string{"有給休暇の申請方法", "年次有給休暇取得について"}, ids(hits)[:2])
	require.Equal(t, float64(9), hits[0].Metadata["watchers"])
	for _, h := range hits[1:] {
		require.NotEqual(t, hits[0].DocumentID, h.DocumentID, "a document contributes once")
	}

	// Strict lexical: every term must appear as a phrase. "領収書 ネットワーク"
	// never co-occur, so the strict plan is empty; lexical-only mode then
	// falls back to any-token (a keyword search never comes back empty when
	// a token exists), while hybrid skips the fallback and lets the vector
	// leg rank — so a Wi-Fi document is not pulled up by one shared bigram.
	loose := ids(search(`{"query":"領収書 ネットワーク","mode":"lexical"}`))
	require.ElementsMatch(t, []string{"経費精算", "社内Wi-Fi"}, loose)
	hybrid := search(`{"query":"領収書 ネットワーク","mode":"hybrid"}`)
	require.NotEmpty(t, hybrid)
	for _, h := range hybrid {
		require.Less(t, h.Score, 2/float64(61), "no document received a lexical contribution on top of the vector rank")
	}

	// Popularity breaks a near-tie in favour of the watched document but
	// never outranks a clear match.
	require.Equal(t, "経費精算", ids(search(`{"query":"領収書","mode":"hybrid"}`))[0])

	// Rerank and expand use a fake chat endpoint: the system prompt decides
	// which call it is; the reranker reverses the order, the expander adds
	// the official term for "休み" that only the Wi-Fi-free documents carry.
	var prompts []string
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, bearer, r.Header.Get("Authorization"))
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		var body struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		prompts = append(prompts, body.Messages[1].Content)
		if strings.Contains(body.Messages[0].Content, "expand") {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"terms\":[\"年次有給休暇\",\"休み\"],\"alternatives\":[\"有給休暇の申請方法\"]}"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"<think>…</think>\n[2, 1]"}}]}`))
	}))
	defer llm.Close()
	for _, body := range []string{`{"query":"有給","rerank":true}`, `{"query":"有給","expand":true}`} {
		w := call(t, h, "POST", "/search", body, bearer)
		require.Equal(t, 503, w.Code, "no LLM configured")
		require.Contains(t, w.Body.String(), "knowledge_llm_unavailable")
	}
	options.LLM = &domain.LLM{BaseURL: llm.URL + "/v1", Model: "m", Client: llm.Client()}
	h = New(db, auth, platform, options)
	w := call(t, h, "POST", "/search", `{"query":"有給","rerank":true,"topK":2}`, bearer)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"reranked":true`)
	require.Contains(t, w.Body.String(), `"expanded":[]`)
	var reranked struct{ Hits []domain.Hit }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &reranked))
	require.Equal(t, []string{"年次有給休暇取得について", "有給休暇の申請方法"}, ids(reranked.Hits))
	require.Len(t, prompts, 1)
	require.Contains(t, prompts[0], "[1] 従業員用ガイド/勤怠/有給休暇の申請方法 (watchers 9)")
	require.Contains(t, prompts[0], "有給休暇の申請は前日までにSlackとKOTで行います。", "the document opening is shown")
	require.NotContains(t, prompts[0], strings.Repeat("有給休暇の申請は前日までにSlackとKOTで行います。", 60), "excerpts are truncated")

	// Expand: the query's own ranking stays on top; formulations only add
	// support (half weight) and are reported. The query itself is never
	// re-sent as a formulation.
	prompts = nil
	w = call(t, h, "POST", "/search", `{"query":"休み","expand":true}`, bearer)
	require.Equal(t, 200, w.Code, w.Body.String())
	var expanded struct {
		Hits     []domain.Hit
		Expanded []string
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &expanded))
	require.Equal(t, []string{"有給休暇の申請方法", "年次有給休暇"}, expanded.Expanded, "alternatives first, the verbatim query dropped")
	// Hash-vector ties in the base query are ordered by random document IDs.
	// Check the fusion contract against each independently retrieved leg,
	// rather than assuming one tied document always takes the first rank.
	expected := search(`{"query":"休み"}`)
	for _, query := range expanded.Expanded {
		body, err := json.Marshal(domain.Search{Query: query})
		require.NoError(t, err)
		for _, alternative := range search(string(body)) {
			for i := range expected {
				if expected[i].DocumentID == alternative.DocumentID {
					expected[i].Score += alternative.Score / 2
					break
				}
			}
		}
	}
	require.ElementsMatch(t, ids(expected), ids(expanded.Hits))
	scores := map[string]float64{}
	for _, hit := range expected {
		scores[hit.DocumentID] = hit.Score
	}
	for i, hit := range expanded.Hits {
		require.InDelta(t, scores[hit.DocumentID], hit.Score, 1e-12)
		if i > 0 {
			require.GreaterOrEqual(t, scores[expanded.Hits[i-1].DocumentID], scores[hit.DocumentID])
		}
	}
	require.Len(t, prompts, 1)
	require.Contains(t, prompts[0], "Query: 休み")
	// GET carries the same flags.
	w = call(t, h, "GET", "/search?query=%E6%9C%89%E7%B5%A6&rerank=true&topK=2", "", bearer)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"reranked":true`)
}
func TestAnswerStreamsCitationsTokensAndDone(t *testing.T) {
	for _, model := range []string{"", "custom"} {
		t.Run("model="+model, func(t *testing.T) {
			testAnswerStreamsCitationsTokensAndDone(t, model)
		})
	}
}

func testAnswerStreamsCitationsTokensAndDone(t *testing.T, model string) {
	t.Helper()
	db, options, auth, platform, bearer := fixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = fmt.Fprint(w, `{"data":[{"id":"test"}]}`)
			return
		}
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.Equal(t, bearer, r.Header.Get("Authorization"))
		var input map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		require.Equal(t, true, input["stream"])
		if model == "" {
			require.NotContains(t, input, "model")
		} else {
			require.Equal(t, model, input["model"])
		}
		require.Contains(t, fmt.Sprint(input["messages"]), "Quantum handbook")
		require.Contains(t, fmt.Sprint(input["messages"]), "Answer in the same language as the question.")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Grounded \"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"answer [1]\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	t.Setenv("KNOWLEDGE_LLM_URL", upstream.URL)
	t.Setenv("KNOWLEDGE_LLM_MODEL", model)
	var err error
	options.LLM, err = domain.LLMFromEnv()
	require.NoError(t, err)
	h := New(db, auth, platform, options)
	w := call(t, h, "POST", "/documents", `{"collection":"manual","title":"Quantum handbook","content":"Quantum retrieval handbook"}`, bearer)
	require.Equal(t, 200, w.Code, w.Body.String())
	w = call(t, h, "POST", "/answer", `{"query":"Quantum"}`, bearer)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
	body := w.Body.String()
	require.Contains(t, body, "event: citation\n")
	require.Contains(t, body, "event: token\ndata: {\"text\":\"Grounded \"}")
	require.Contains(t, body, "event: done\ndata: {}\n\n")
	require.Less(t, strings.Index(body, "event: citation"), strings.Index(body, "event: token"))
}

func TestAnswerSlowHeadersDoNotBlockCollections(t *testing.T) {
	db, options, auth, platform, bearer := fixture(t)
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = fmt.Fprint(w, `{"data":[{"id":"stub","appthrust.io/maxPromptTokens":3000}]}`)
			return
		}
		close(started)
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	options.LLM = &domain.LLM{BaseURL: upstream.URL + "/v1", Client: upstream.Client()}
	h := New(db, auth, platform, options)
	for _, title := range []string{"first", "second"} {
		w := call(t, h, "POST", "/documents", `{"collection":"manual","title":"`+title+`","content":"Quantum handbook"}`, bearer)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/api/knowledge/v1/answer", strings.NewReader(`{"query":"Quantum"}`)).WithContext(ctx)
	r.Header.Set("Authorization", bearer)
	r.Header.Set("Content-Type", "application/json")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		done <- w
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Ask did not reach the LLM")
	}
	server := httptest.NewServer(h)
	defer server.Close()
	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/knowledge/v1/collections", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", bearer)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	res, err := client.Do(req)
	require.NoError(t, err, "collections must respond within 500 ms while Ask waits for headers")
	res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	select {
	case <-done:
		t.Fatal("Ask finished before the collections probe")
	default:
	}
	select {
	case w := <-done:
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "event: done\n")
	case <-time.After(5 * time.Second):
		t.Fatal("Ask did not finish after the LLM sent headers")
	}
}

func TestAnswerBudgetPreservesCitationPrefix(t *testing.T) {
	db, options, auth, platform, bearer := fixture(t)
	prompts := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"test","appthrust.io/maxPromptTokens":350}]}`)
		case "/v1/chat/completions":
			var input struct {
				Messages []struct{ Role, Content string }
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, message := range input.Messages {
				if message.Role == "user" {
					prompts <- message.Content
				}
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer [1]\"}}]}\n\ndata: [DONE]\n\n")
		default:
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	options.LLM = &domain.LLM{BaseURL: upstream.URL + "/v1", Model: "test", Client: upstream.Client()}
	h := New(db, auth, platform, options)
	for i := range 3 {
		body, err := json.Marshal(map[string]string{
			"collection": "manual",
			"title":      fmt.Sprintf("Quantum handbook %d", i),
			"url":        fmt.Sprintf("https://example.com/%d", i),
			"content":    "Quantum " + strings.Repeat("参照資料です。", 100),
		})
		require.NoError(t, err)
		w := call(t, h, "POST", "/documents", string(body), bearer)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	w := call(t, h, "POST", "/answer", `{"query":"Quantum"}`, bearer)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "event: done\n")
	var citations []domain.Hit
	for _, event := range strings.Split(w.Body.String(), "\n\n") {
		if payload, ok := strings.CutPrefix(event, "event: citation\ndata: "); ok {
			var hit domain.Hit
			require.NoError(t, json.Unmarshal([]byte(payload), &hit))
			citations = append(citations, hit)
		}
	}
	require.Len(t, citations, 3, "the handler still emits all retrieval results before the tokens")
	var prompt string
	select {
	case prompt = <-prompts:
	default:
		t.Fatal("no user message sent to the upstream")
	}
	header := fmt.Sprintf("Excerpts:\n\n[1] %s (%s)\n", citations[0].Title, citations[0].URL)
	require.True(t, strings.HasPrefix(prompt, header), "prompt numbering must address the original citation prefix")
	require.NotContains(t, prompt, "\n[2]")
	text := strings.TrimSuffix(strings.TrimPrefix(prompt, header), "\n\nQuestion: Quantum")
	require.NotEmpty(t, text)
	require.True(t, strings.HasPrefix(citations[0].Text, text))
	require.Less(t, len(text), len(citations[0].Text))
	require.Contains(t, w.Body.String(), `{"text":"answer [1]"}`)
}

func TestLLMFailureReportsPromptBudgetAsClientError(t *testing.T) {
	w := httptest.NewRecorder()
	err := fmt.Errorf("starting Ask: %w", &domain.PromptBudgetError{MaxPromptTokens: 2048})
	httpx.WriteError(w, llmFailure(err, "knowledge_llm_error"), "knowledge")
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.JSONEq(t, `{"error":"knowledge_prompt_budget_exceeded","message":"Question and instructions exceed the LLM prompt budget of 2048 tokens"}`, w.Body.String())
}

func TestAnswerRejectsOversizedQuestionBeforeStreaming(t *testing.T) {
	for _, advertised := range []bool{false, true} {
		t.Run(fmt.Sprintf("advertised=%t", advertised), func(t *testing.T) {
			db, options, auth, platform, bearer := fixture(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Errorf("over-budget prompt reached upstream: %s", r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if advertised {
					_, _ = fmt.Fprint(w, `{"data":[{"id":"test","appthrust.io/maxPromptTokens":3072}]}`)
				} else {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer upstream.Close()
			options.LLM = &domain.LLM{BaseURL: upstream.URL + "/v1", Model: "test", Client: upstream.Client()}
			h := New(db, auth, platform, options)
			body, err := json.Marshal(map[string]string{"query": strings.Repeat("問", 4096)})
			require.NoError(t, err)
			w := call(t, h, "POST", "/answer", string(body), bearer)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.Equal(t, "application/json", w.Header().Get("Content-Type"))
			budget := 2048
			if advertised {
				budget = 3072
			}
			require.JSONEq(t, fmt.Sprintf(`{"error":"knowledge_prompt_budget_exceeded","message":"Question and instructions exceed the LLM prompt budget of %d tokens"}`, budget), w.Body.String())
			require.NotContains(t, w.Body.String(), "event:")
		})
	}
}

func TestAnswerUpstreamFailures(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			db, options, auth, platform, bearer := fixture(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/models" {
					_, _ = fmt.Fprint(w, `{"data":[]}`)
					return
				}
				var input map[string]any
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Errorf("decode request: %v", err)
					return
				}
				if !streaming {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Partial answer\"}}]}\n\n")
				w.(http.Flusher).Flush()
				// A canceled idle read and an interrupted HTTP body both surface
				// through StreamTokens as "LLM stream interrupted".
				panic(http.ErrAbortHandler)
			}))
			defer upstream.Close()
			t.Setenv("KNOWLEDGE_LLM_URL", upstream.URL)
			t.Setenv("KNOWLEDGE_LLM_MODEL", "test")
			var err error
			options.LLM, err = domain.LLMFromEnv()
			require.NoError(t, err)
			h := New(db, auth, platform, options)
			w := call(t, h, "POST", "/documents", `{"collection":"manual","title":"Quantum handbook","content":"Quantum retrieval handbook"}`, bearer)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			w = call(t, h, "POST", "/answer", `{"query":"Quantum"}`, bearer)
			if !streaming {
				require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
				require.JSONEq(t, `{"error":"knowledge_llm_error","message":"The LLM provider is unavailable"}`, w.Body.String())
				return
			}
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
			require.Contains(t, w.Body.String(), "event: token\ndata: {\"text\":\"Partial answer\"}")
			require.Contains(t, w.Body.String(), "event: error\ndata: {\"error\":\"knowledge_llm_error\",\"message\":\"The LLM stream did not complete\"}")
			require.NotContains(t, w.Body.String(), "event: done\n")
		})
	}
}

func TestGatewayFailuresBeforeStreaming(t *testing.T) {
	db, options, auth, platform, bearer := fixture(t)
	h := New(db, auth, platform, options)
	for _, doc := range []string{
		`{"collection":"manual","title":"Quantum handbook","content":"Quantum retrieval handbook"}`,
		`{"collection":"manual","title":"Quantum guide","content":"Quantum retrieval guide"}`,
	} {
		w := call(t, h, "POST", "/documents", doc, bearer)
		require.Equal(t, 200, w.Code, w.Body.String())
	}
	w := call(t, h, "POST", "/answer", `{"query":"Quantum"}`, bearer)
	require.Equal(t, 503, w.Code)
	require.Contains(t, w.Body.String(), "knowledge_llm_unavailable")
	for _, tc := range []struct {
		name, code string
		status     int
	}{
		{name: "model denied", status: 403, code: "ai_model_denied"},
		{name: "policy pending", status: 403, code: "ai_policy_pending"},
		{name: "logging unavailable", status: 503, code: "ai_policy_logging_unavailable"},
		{name: "provider error", status: 502, code: "ai_provider_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, bearer, r.Header.Get("Authorization"))
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": tc.code, "message": "private provider detail"}})
			}))
			defer gateway.Close()
			options.LLM = &domain.LLM{BaseURL: gateway.URL + "/v1", Client: gateway.Client()}
			h := New(db, auth, platform, options)
			for _, request := range []struct{ path, body, failure string }{
				{"/answer", `{"query":"Quantum"}`, "knowledge_llm_error"},
				{"/search", `{"query":"Quantum","mode":"lexical","rerank":true}`, "knowledge_llm_unavailable"},
				{"/search", `{"query":"Quantum","expand":true}`, "knowledge_llm_unavailable"},
			} {
				w := call(t, h, "POST", request.path, request.body, bearer)
				require.Equal(t, "application/json", w.Header().Get("Content-Type"))
				if tc.status == 403 {
					require.Equal(t, 403, w.Code, w.Body.String())
					require.Contains(t, w.Body.String(), `"error":"knowledge_llm_denied"`)
					require.Contains(t, w.Body.String(), tc.code)
				} else {
					require.Equal(t, 502, w.Code, w.Body.String())
					require.Contains(t, w.Body.String(), request.failure)
				}
				require.NotContains(t, w.Body.String(), "private provider detail")
				require.NotContains(t, w.Body.String(), "event:")
			}
		})
	}
}

func TestNotionSyncPersistsDocumentsAndNeverReturnsToken(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprintf("platformManaged=%t", managed), func(t *testing.T) {
			testNotionSync(t, managed)
		})
	}
}
func testNotionSync(t *testing.T, managed bool) {
	db, options, auth, platform, bearer := fixture(t)
	blockCalls := 0
	notion := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer notion-secret", r.Header.Get("Authorization"))
		require.Equal(t, "2022-06-28", r.Header.Get("Notion-Version"))
		switch r.URL.Path {
		case "/v1/search":
			_, _ = w.Write([]byte(`{"results":[{"id":"page","object":"page","last_edited_time":"2026-10-14","properties":{"Name":{"title":[{"plain_text":"Notion handbook"}]}}}],"has_more":false}`))
		case "/v1/blocks/page/children":
			blockCalls++
			if r.URL.Query().Get("start_cursor") == "next" {
				_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"nested","type":"paragraph","has_children":true,"paragraph":{"rich_text":[{"plain_text":"Notion knowledge"}]}}],"has_more":true,"next_cursor":"next"}`))
		case "/v1/blocks/nested/children":
			_, _ = w.Write([]byte(`{"results":[{"id":"text","type":"paragraph","paragraph":{"rich_text":[{"plain_text":"Nested retrieval"}]}}],"has_more":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer notion.Close()
	options.Notion = &domain.Notion{BaseURL: notion.URL, Client: notion.Client()}
	tokenSource := "stored"
	input := `{"type":"notion","collection":"manual","config":{"token":"notion-secret"}}`
	if managed {
		options.NotionToken = "notion-secret"
		tokenSource = "platform"
		input = `{"type":"notion","collection":"manual","config":{}}`
	}
	h := New(db, auth, platform, options)
	stopWorker, err := StartWorker(db, options)
	require.NoError(t, err)
	defer stopWorker()
	if managed {
		for _, token := range []string{`"notion-secret"`, `""`, `null`, `123`} {
			rejected := call(t, h, "POST", "/sources", `{"type":"notion","collection":"manual","config":{"token":`+token+`}}`, jwt("admin"))
			require.Equal(t, 400, rejected.Code)
			require.Contains(t, rejected.Body.String(), "knowledge_notion_token_platform_managed")
			require.Contains(t, rejected.Body.String(), "The Notion token is managed in the Project's built-in app settings")
			require.NotContains(t, rejected.Body.String(), "notion-secret")
		}
	}
	w := call(t, h, "POST", "/sources", input, jwt("admin"))
	require.Equal(t, 200, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), "notion-secret")
	require.NotContains(t, w.Body.String(), `"token"`)
	var source domain.Source
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &source))
	require.Equal(t, tokenSource, source.TokenSource)
	require.NoError(t, db.Do(context.Background(), false, func(tx *store.Tx) error {
		stored, err := tx.Source(source.ID)
		if managed {
			require.Empty(t, stored.Config.Token, "platform tokens must not be persisted")
		} else {
			require.Equal(t, "notion-secret", stored.Config.Token)
		}
		return err
	}))
	if managed {
		// Previously stored credentials are never used when the platform owns
		// the integration. The fake Notion server below checks the env token.
		require.NoError(t, db.Do(context.Background(), true, func(tx *store.Tx) error {
			return tx.Exec(`UPDATE sources SET config = config || '{"token":"obsolete-stored-token"}'::jsonb WHERE id=$1`, source.ID)
		}))
	}
	for i := range 2 {
		w = call(t, h, "POST", "/sources/"+source.ID+"/sync", "{}", jwt("admin"))
		require.Equal(t, 202, w.Code, w.Body.String())
		result := waitSync(t, h, w).Result
		require.Empty(t, result.Errors)
		require.Equal(t, 1-i, result.Documents)
	}
	require.Equal(t, 2, blockCalls)
	for _, path := range []string{"/sources", "/sources/" + source.ID} {
		w = call(t, h, "GET", path, "", jwt("admin"))
		require.Equal(t, 200, w.Code)
		require.NotContains(t, w.Body.String(), "notion-secret")
		require.NotContains(t, w.Body.String(), `"token"`)
		require.Contains(t, w.Body.String(), "finishedAt")
		require.Contains(t, w.Body.String(), `"tokenSource":"`+tokenSource+`"`)
	}
	w = call(t, h, "POST", "/search", `{"query":"Nested retrieval"}`, bearer)
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "Notion handbook")
	w = call(t, h, "GET", "/sources", "", bearer)
	require.Equal(t, 403, w.Code)
}
func TestEsaSyncPersistsDocumentsAndNeverReturnsToken(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprintf("platformManaged=%t", managed), func(t *testing.T) {
			testEsaSync(t, managed)
		})
	}
}
func testEsaSync(t *testing.T, managed bool) {
	db, options, auth, platform, bearer := fixture(t)
	calls := 0
	esa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer esa-secret", r.Header.Get("Authorization"))
		require.Equal(t, "/v1/teams/docs/posts", r.URL.Path)
		require.Equal(t, "tag:api wip:false -in:Archived", r.URL.Query().Get("q"))
		calls++
		_, _ = w.Write([]byte(`{"posts":[{"number":12,"name":"esa handbook","category":"Engineering","tags":["api"],"body_md":"Nested retrieval from esa","url":"https://docs.esa.io/posts/12","updated_at":"2026-10-15T00:00:00+09:00"}],"next_page":null}`))
	}))
	defer esa.Close()
	options.Esa = &domain.Esa{BaseURL: esa.URL, Client: esa.Client()}
	tokenSource := "stored"
	input := `{"type":"esa","collection":"manual","config":{"token":"esa-secret","team":"docs","query":"tag:api"}}`
	if managed {
		options.EsaToken = "esa-secret"
		tokenSource = "platform"
		input = `{"type":"esa","collection":"manual","config":{"team":"docs","query":" tag:api "}}`
	}
	h := New(db, auth, platform, options)
	stopWorker, err := StartWorker(db, options)
	require.NoError(t, err)
	defer stopWorker()
	if managed {
		rejected := call(t, h, "POST", "/sources", `{"type":"esa","collection":"manual","config":{"token":"esa-secret","team":"docs"}}`, jwt("admin"))
		require.Equal(t, 400, rejected.Code)
		require.Contains(t, rejected.Body.String(), "knowledge_esa_token_platform_managed")
		require.Contains(t, rejected.Body.String(), "The esa token is managed in the Project's built-in app settings")
		require.NotContains(t, rejected.Body.String(), "esa-secret")
		// An esa binding does not make Notion platform-managed.
		notion := call(t, h, "POST", "/sources", `{"type":"notion","collection":"manual","config":{"token":"notion-stored"}}`, jwt("admin"))
		require.Equal(t, 200, notion.Code, notion.Body.String())
		require.Contains(t, notion.Body.String(), `"tokenSource":"stored"`)
	}
	for _, bad := range []string{`{"type":"esa","collection":"manual","config":{"token":"x","team":"Docs"}}`, `{"type":"esa","collection":"manual","config":{"token":"x","team":"docs","query":"a\nb"}}`, `{"type":"wiki","collection":"manual","config":{"token":"x"}}`} {
		w := call(t, h, "POST", "/sources", bad, jwt("admin"))
		require.Equal(t, 400, w.Code, bad)
	}
	w := call(t, h, "POST", "/sources", input, jwt("admin"))
	require.Equal(t, 200, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), "esa-secret")
	require.NotContains(t, w.Body.String(), `"token"`)
	var source domain.Source
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &source))
	require.Equal(t, tokenSource, source.TokenSource)
	require.Equal(t, "esa", source.Type)
	require.Equal(t, "docs", source.Config.Team)
	require.Equal(t, "tag:api", source.Config.Query)
	require.False(t, source.Config.IncludeWIP)
	require.NoError(t, db.Do(context.Background(), false, func(tx *store.Tx) error {
		stored, err := tx.Source(source.ID)
		if managed {
			require.Empty(t, stored.Config.Token)
		} else {
			require.Equal(t, "esa-secret", stored.Config.Token)
		}
		return err
	}))
	for i := range 2 {
		w = call(t, h, "POST", "/sources/"+source.ID+"/sync", "{}", jwt("admin"))
		require.Equal(t, 202, w.Code, w.Body.String())
		result := waitSync(t, h, w).Result
		require.Empty(t, result.Errors)
		require.Equal(t, 1-i, result.Documents)
	}
	require.Equal(t, 2, calls)
	w = call(t, h, "GET", "/sources/"+source.ID, "", jwt("admin"))
	require.Equal(t, 200, w.Code)
	require.NotContains(t, w.Body.String(), "esa-secret")
	require.Contains(t, w.Body.String(), `"tokenSource":"`+tokenSource+`"`)
	w = call(t, h, "POST", "/search", `{"query":"Nested retrieval"}`, bearer)
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "esa handbook")
	require.Contains(t, w.Body.String(), "https://docs.esa.io/posts/12")
	w = call(t, h, "GET", "/documents?collection=manual", "", jwt("admin"))
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"externalId":"esa:`+source.ID+`:12"`)
	require.Contains(t, w.Body.String(), `"category":"Engineering"`)
}
