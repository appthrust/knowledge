package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/appthrust/knowledge/cmd/server/internal/store"
	"github.com/stretchr/testify/require"
)

func queuedJob(t *testing.T, w *httptest.ResponseRecorder) domain.IngestJob {
	t.Helper()
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	var response struct {
		Job domain.IngestJob `json:"job"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.NotEmpty(t, response.Job.ID)
	return response.Job
}

func waitJob(t *testing.T, h http.Handler, id string) domain.IngestJob {
	t.Helper()
	var job domain.IngestJob
	require.Eventually(t, func() bool {
		w := call(t, h, "GET", "/jobs/"+id, "", jwt("admin"))
		require.Equal(t, 200, w.Code, w.Body.String())
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &job))
		return job.State == "done" || job.State == "failed"
	}, 10*time.Second, 10*time.Millisecond)
	return job
}

func waitSync(t *testing.T, h http.Handler, w *httptest.ResponseRecorder) domain.IngestJob {
	t.Helper()
	job := waitJob(t, h, queuedJob(t, w).ID)
	require.Equal(t, "done", job.State, job.Error)
	require.NotNil(t, job.Result)
	return job
}

func createEsaSource(t *testing.T, h http.Handler) domain.Source {
	t.Helper()
	w := call(t, h, "POST", "/sources", `{"type":"esa","collection":"manual","config":{"token":"secret","team":"docs"}}`, jwt("admin"))
	require.Equal(t, 200, w.Code, w.Body.String())
	var source domain.Source
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &source))
	require.Nil(t, source.Sync)
	return source
}

func TestSyncJobsIdempotencyProgressAndDocumentErrors(t *testing.T) {
	db, options, auth, platform, bearer := fixture(t)
	secondPage := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			close(secondPage)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte(`{"posts":[{"number":2,"name":"Second","body_md":"Second document","updated_at":"1"}],"next_page":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"posts":[{"number":1,"name":"First","body_md":"First document","updated_at":"1"},{"number":0}],"next_page":2}`))
	}))
	defer upstream.Close()
	options.Esa = &domain.Esa{BaseURL: upstream.URL, Client: upstream.Client()}
	h := New(db, auth, platform, options)
	source := createEsaSource(t, h)
	path := "/sources/" + source.ID + "/sync"
	first := queuedJob(t, call(t, h, "POST", path, "{}", jwt("admin")))
	require.Equal(t, "queued", first.State)
	require.Nil(t, first.StartedAt)
	require.Nil(t, first.Progress.Total)
	require.Equal(t, first, queuedJob(t, call(t, h, "POST", path, "{}", jwt("admin"))))
	stop, err := StartWorker(db, options)
	require.NoError(t, err)
	defer stop()
	defer unblock()
	select {
	case <-secondPage:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reach second page")
	}
	running := queuedJob(t, call(t, h, "POST", path, "{}", jwt("admin")))
	require.Equal(t, first.ID, running.ID)
	require.Equal(t, "running", running.State)
	require.Equal(t, 1, running.Progress.Documents)
	require.Equal(t, 1, running.Progress.Chunks)
	w := call(t, h, "GET", "/sources/"+source.ID, "", jwt("admin"))
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &source))
	require.Equal(t, running.ID, source.Sync.ID)
	require.Nil(t, source.LastSync)
	unblock()
	job := waitJob(t, h, first.ID)
	require.Equal(t, "done", job.State)
	require.Equal(t, 2, job.Progress.Documents)
	require.Equal(t, 2, job.Result.Documents)
	require.Equal(t, []string{"post 0: invalid esa post number"}, job.Result.Errors)
	require.NotNil(t, job.StartedAt)
	require.NotNil(t, job.FinishedAt)
	require.Nil(t, job.Error)
	w = call(t, h, "GET", "/sources/"+source.ID, "", jwt("admin"))
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &source))
	require.Equal(t, job.Result, source.LastSync)
	require.Equal(t, job.ID, source.Sync.ID)
	w = call(t, h, "GET", "/jobs/"+job.ID, "", bearer)
	require.Equal(t, 200, w.Code, w.Body.String())
	encoded, err := json.Marshal(job)
	require.NoError(t, err)
	t.Logf("completed sync job: %s", encoded)
}

func TestSyncJobWalkFailurePreservesLastSync(t *testing.T) {
	db, options, auth, platform, _ := fixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer upstream.Close()
	options.Esa = &domain.Esa{BaseURL: upstream.URL, Client: upstream.Client()}
	h := New(db, auth, platform, options)
	source := createEsaSource(t, h)
	previous := domain.SyncResult{Documents: 4, Chunks: 5, Errors: []string{}, FinishedAt: time.Now().UTC()}
	require.NoError(t, db.Do(context.Background(), true, func(tx *store.Tx) error { return tx.SyncResult(source.ID, previous) }))
	stop, err := StartWorker(db, options)
	require.NoError(t, err)
	defer stop()
	job := queuedJob(t, call(t, h, "POST", "/sources/"+source.ID+"/sync", "{}", jwt("admin")))
	job = waitJob(t, h, job.ID)
	require.Equal(t, "failed", job.State)
	require.NotNil(t, job.Error)
	require.Contains(t, *job.Error, "503")
	require.NoError(t, db.Do(context.Background(), false, func(tx *store.Tx) error {
		source, err := tx.Source(source.ID)
		require.Equal(t, 4, source.LastSync.Documents)
		require.Equal(t, job.ID, source.Sync.ID)
		return err
	}))
}

func TestSyncJobRequeuesAfterStoreReopen(t *testing.T) {
	db, options, auth, platform, _ := fixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"posts":[{"number":1,"name":"Resumed","body_md":"Resumed document","updated_at":"1"}],"next_page":null}`))
	}))
	defer upstream.Close()
	options.Esa = &domain.Esa{BaseURL: upstream.URL, Client: upstream.Client()}
	h := New(db, auth, platform, options)
	source := createEsaSource(t, h)
	job := queuedJob(t, call(t, h, "POST", "/sources/"+source.ID+"/sync", "{}", jwt("admin")))
	require.NoError(t, db.Do(context.Background(), true, func(tx *store.Tx) error {
		claimed, err := tx.ClaimJob()
		require.Equal(t, job.ID, claimed.ID)
		require.Equal(t, "running", claimed.State)
		return err
	}))
	require.NoError(t, db.Close())
	reopened, err := store.Open(context.Background(), dsns[db])
	require.NoError(t, err)
	defer reopened.Close()
	require.NoError(t, reopened.Do(context.Background(), false, func(tx *store.Tx) error {
		resumed, err := tx.Job(job.ID)
		require.Equal(t, "queued", resumed.State)
		require.NotNil(t, resumed.StartedAt)
		return err
	}))
	stop, err := StartWorker(reopened, options)
	require.NoError(t, err)
	defer stop()
	h = New(reopened, auth, platform, options)
	job = waitJob(t, h, job.ID)
	require.Equal(t, "done", job.State)
	require.Equal(t, 1, job.Result.Documents)
}

func TestReembedProviderChangeBatchesTitlesAndVectorSearch(t *testing.T) {
	db, options, auth, platform, bearer := fixture(t)
	h := New(db, auth, platform, options)
	for i := range 10 {
		title := fmt.Sprintf("Equipment %d", i)
		if i == 0 {
			title = "Leave policy"
		}
		body, err := json.Marshal(map[string]string{"collection": "manual", "externalId": fmt.Sprint(i), "title": title, "content": "Handbook details"})
		require.NoError(t, err)
		w := call(t, h, "POST", "/documents", string(body), bearer)
		require.Equal(t, 200, w.Code, w.Body.String())
	}
	require.NoError(t, db.Close())
	reopened, err := store.Open(context.Background(), dsns[db])
	require.NoError(t, err)
	defer reopened.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		require.Equal(t, "/v1/embeddings", r.URL.Path)
		require.Empty(t, r.Header.Get("Authorization"))
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "embeddinggemma", body.Model)
		data := []map[string]any{}
		for i, input := range body.Input {
			require.True(t, strings.HasPrefix(input, "title: ") || strings.HasPrefix(input, "task: search result | query: "), input)
			v := make([]float32, 16)
			if strings.Contains(input, "Leave") {
				v[0] = 1
			} else {
				v[1] = 1
			}
			data = append(data, map[string]any{"index": i, "embedding": v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer upstream.Close()
	defer unblock()
	options.Embedding = &domain.Embedder{Provider: "openai", Model: "embeddinggemma", BaseURL: upstream.URL + "/v1", Client: upstream.Client(), QueryTemplate: "task: search result | query: {text}", DocumentTemplate: "title: {title} | text: {text}"}
	require.NoError(t, reopened.Do(context.Background(), true, func(tx *store.Tx) error {
		candidates, err := tx.ReembedCandidates(options.Embedding.IndexProvider())
		require.Len(t, candidates, 8)
		// The most recently written document converts first.
		require.Equal(t, "9", candidates[0].ExternalID)
		require.Equal(t, "2", candidates[7].ExternalID)
		if err != nil {
			return err
		}
		if err = tx.EnsureReembed(options.Embedding.IndexProvider()); err != nil {
			return err
		}
		return tx.EnsureReembed(options.Embedding.IndexProvider())
	}))
	config, err := EmbeddingConfig(context.Background(), reopened, options.Embedding)
	require.NoError(t, err)
	require.Equal(t, domain.Backfill{Pending: 10, State: "queued"}, config.Backfill)
	stop, err := StartWorker(reopened, options)
	require.NoError(t, err)
	defer stop()
	defer unblock()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("backfill did not call embeddings")
	}
	config, err = EmbeddingConfig(context.Background(), reopened, options.Embedding)
	require.NoError(t, err)
	require.Equal(t, domain.Backfill{Pending: 10, State: "running"}, config.Backfill)
	var id string
	require.NoError(t, reopened.Do(context.Background(), false, func(tx *store.Tx) error {
		var count int
		require.NoError(t, tx.SQL.QueryRowContext(tx.Context, `SELECT count(*) FROM ingest_jobs WHERE kind='reembed'`).Scan(&count))
		require.Equal(t, 1, count)
		return tx.SQL.QueryRowContext(tx.Context, `SELECT id FROM ingest_jobs WHERE kind='reembed'`).Scan(&id)
	}))
	unblock()
	h = New(reopened, auth, platform, options)
	job := waitJob(t, h, id)
	require.Equal(t, "done", job.State, job.Error)
	require.Equal(t, 10, job.Progress.Documents)
	require.Equal(t, 10, *job.Progress.Total)
	config, err = EmbeddingConfig(context.Background(), reopened, options.Embedding)
	require.NoError(t, err)
	require.Equal(t, 16, config.Dimensions)
	require.Equal(t, domain.Backfill{Pending: 0, State: "idle"}, config.Backfill)
	w := call(t, h, "POST", "/search", `{"query":"Leave","mode":"vector","topK":1}`, bearer)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "Leave policy")
}

// esaUpstream serves whatever post list the test currently sets.
type esaUpstream struct {
	mu    sync.Mutex
	posts string
}

func (u *esaUpstream) set(posts string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.posts = posts
}

func (u *esaUpstream) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, _ = w.Write([]byte(`{"posts":[` + u.posts + `],"next_page":null}`))
}

func documentTitles(t *testing.T, h http.Handler) []string {
	t.Helper()
	w := call(t, h, "GET", "/documents?collection=manual", "", jwt("admin"))
	require.Equal(t, 200, w.Code, w.Body.String())
	var response struct {
		Data []domain.Document `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	titles := []string{}
	for _, d := range response.Data {
		titles = append(titles, d.Title)
	}
	return titles
}

func TestSyncPrunesDocumentsTheSourceNoLongerReturns(t *testing.T) {
	db, options, auth, platform, bearer := fixture(t)
	posts := &esaUpstream{}
	upstream := httptest.NewServer(posts)
	defer upstream.Close()
	options.Esa = &domain.Esa{BaseURL: upstream.URL, Client: upstream.Client()}
	h := New(db, auth, platform, options)
	source := createEsaSource(t, h)
	stop, err := StartWorker(db, options)
	require.NoError(t, err)
	defer stop()
	sync := func() domain.IngestJob {
		return waitSync(t, h, call(t, h, "POST", "/sources/"+source.ID+"/sync", "{}", jwt("admin")))
	}
	posts.set(`{"number":1,"name":"Kept","body_md":"Kept","updated_at":"1"},{"number":2,"name":"Archived later","body_md":"Old","updated_at":"1"}`)
	job := sync()
	require.Equal(t, 2, job.Result.Documents)
	require.Equal(t, 0, job.Result.Removed)
	require.ElementsMatch(t, []string{"Kept", "Archived later"}, documentTitles(t, h))

	// A per-post error leaves the snapshot untrusted: nothing is removed.
	posts.set(`{"number":1,"name":"Kept","body_md":"Kept","updated_at":"1"},{"number":0}`)
	job = sync()
	require.Equal(t, 0, job.Result.Documents)
	require.Equal(t, 0, job.Result.Removed)
	require.Equal(t, []string{"post 0: invalid esa post number"}, job.Result.Errors)
	require.ElementsMatch(t, []string{"Kept", "Archived later"}, documentTitles(t, h))

	// A failed walk never prunes either.
	posts.set(`garbage`)
	failed := waitJob(t, h, queuedJob(t, call(t, h, "POST", "/sources/"+source.ID+"/sync", "{}", jwt("admin"))).ID)
	require.Equal(t, "failed", failed.State)
	require.ElementsMatch(t, []string{"Kept", "Archived later"}, documentTitles(t, h))

	// A clean walk that no longer returns post 2 removes it, unchanged post 1 stays.
	posts.set(`{"number":1,"name":"Kept","body_md":"Kept","updated_at":"1"}`)
	job = sync()
	require.Equal(t, 0, job.Result.Documents)
	require.Equal(t, 1, job.Result.Removed)
	require.Equal(t, []string{"Kept"}, documentTitles(t, h))
	w := call(t, h, "GET", "/sources/"+source.ID, "", jwt("admin"))
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &source))
	require.Equal(t, 1, source.LastSync.Removed)

	// Documents of another source in the same collection are never pruned.
	manual := call(t, h, "POST", "/documents", `{"collection":"manual","title":"Manual","content":"Manual"}`, bearer)
	require.Equal(t, 200, manual.Code, manual.Body.String())
	posts.set(``)
	job = sync()
	require.Equal(t, 1, job.Result.Removed)
	require.Equal(t, []string{"Manual"}, documentTitles(t, h))
}

func TestDeleteSourceRemovesItsDocumentsAndRefusesWhileSyncing(t *testing.T) {
	db, options, auth, platform, bearer := fixture(t)
	posts := &esaUpstream{posts: `{"number":1,"name":"First","body_md":"First","updated_at":"1"},{"number":2,"name":"Second","body_md":"Second","updated_at":"1"}`}
	upstream := httptest.NewServer(posts)
	defer upstream.Close()
	options.Esa = &domain.Esa{BaseURL: upstream.URL, Client: upstream.Client()}
	h := New(db, auth, platform, options)
	source := createEsaSource(t, h)
	manual := call(t, h, "POST", "/documents", `{"collection":"manual","title":"Manual","content":"Manual"}`, bearer)
	require.Equal(t, 200, manual.Code, manual.Body.String())
	// Queued but not yet claimed: no worker is running.
	queuedJob(t, call(t, h, "POST", "/sources/"+source.ID+"/sync", "{}", jwt("admin")))
	w := call(t, h, "DELETE", "/sources/"+source.ID, "", jwt("admin"))
	require.Equal(t, 409, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "knowledge_source_syncing")
	stop, err := StartWorker(db, options)
	require.NoError(t, err)
	defer stop()
	require.Eventually(t, func() bool {
		w := call(t, h, "GET", "/sources/"+source.ID, "", jwt("admin"))
		return json.Unmarshal(w.Body.Bytes(), &source) == nil && source.Sync != nil && source.Sync.State == "done"
	}, 10*time.Second, 10*time.Millisecond)
	require.ElementsMatch(t, []string{"First", "Second", "Manual"}, documentTitles(t, h))
	w = call(t, h, "DELETE", "/sources/"+source.ID, "", bearer)
	require.Equal(t, 403, w.Code)
	w = call(t, h, "DELETE", "/sources/"+source.ID, "", jwt("admin"))
	require.Equal(t, 200, w.Code, w.Body.String())
	require.JSONEq(t, `{"deleted":true,"documents":2}`, w.Body.String())
	require.Equal(t, []string{"Manual"}, documentTitles(t, h))
	require.Equal(t, 404, call(t, h, "GET", "/sources/"+source.ID, "", jwt("admin")).Code)
	require.Equal(t, 404, call(t, h, "DELETE", "/sources/"+source.ID, "", jwt("admin")).Code)
	require.Equal(t, 404, call(t, h, "POST", "/sources/"+source.ID+"/sync", "{}", jwt("admin")).Code)
}
