package domain

import (
	"math"
	"time"
)

const MaxContentBytes = 1 << 20

type Visibility struct {
	Groups []string `json:"groups"`
}
type Collection struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Visibility  Visibility `json:"visibility"`
}
type Document struct {
	ID         string         `json:"id"`
	Collection string         `json:"collection"`
	ExternalID string         `json:"externalId,omitempty"`
	Title      string         `json:"title"`
	URL        string         `json:"url"`
	Content    string         `json:"content"`
	Metadata   map[string]any `json:"metadata"`
	Chunks     int            `json:"chunks"`
}
type Chunk struct {
	Text      string
	Embedding []float32
}
type Hit struct {
	DocumentID string         `json:"documentId"`
	Title      string         `json:"title"`
	URL        string         `json:"url"`
	Collection string         `json:"collection"`
	ChunkIndex int            `json:"chunkIndex"`
	Text       string         `json:"text"`
	Score      float64        `json:"score"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// Popularity is a document prior from connector metadata (esa watchers and
// stars, Notion has none): 0 for unknown, growing with log of the audience
// and saturating at 1 around fifty watchers.
func Popularity(metadata map[string]any) float64 {
	n := number(metadata["watchers"]) + 2*number(metadata["stars"])
	if n <= 0 {
		return 0
	}
	return min(1, math.Log1p(n)/math.Log(50))
}

func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

type Search struct {
	Query       string   `json:"query"`
	Collections []string `json:"collections,omitempty"`
	TopK        int      `json:"topK,omitempty"`
	Mode        string   `json:"mode,omitempty"`
	// Rerank asks the configured LLM to order the retrieved documents and
	// Expand asks it for official terms/rephrasings fused into retrieval;
	// 503 knowledge_llm_unavailable without an LLM.
	Rerank bool `json:"rerank,omitempty"`
	Expand bool `json:"expand,omitempty"`
}
type SourceConfig struct {
	Token string `json:"token,omitempty"`
	// Notion
	RootPageIDs []string `json:"rootPageIds,omitempty"`
	DatabaseIDs []string `json:"databaseIds,omitempty"`
	// esa
	Team            string `json:"team,omitempty"`
	Query           string `json:"query,omitempty"`
	IncludeWIP      bool   `json:"includeWip,omitempty"`
	IncludeArchived bool   `json:"includeArchived,omitempty"`
}

// SyncResult counts the documents a sync wrote and, once the walk completed
// without errors, the documents it removed because the source no longer
// returned them.
type SyncResult struct {
	Documents  int       `json:"documents"`
	Chunks     int       `json:"chunks"`
	Removed    int       `json:"removed"`
	Errors     []string  `json:"errors"`
	FinishedAt time.Time `json:"finishedAt"`
}
type Source struct {
	ID          string       `json:"id"`
	Type        string       `json:"type"`
	Collection  string       `json:"collection"`
	Config      SourceConfig `json:"config"`
	LastSync    *SyncResult  `json:"lastSync"`
	Sync        *IngestJob   `json:"sync"`
	TokenSource string       `json:"tokenSource,omitempty"`
}

// IngestJob is persisted independently of the request that queued it.
type IngestJob struct {
	ID         string      `json:"id"`
	Kind       string      `json:"kind"`
	SourceID   string      `json:"-"`
	State      string      `json:"state"`
	Progress   JobProgress `json:"progress"`
	Result     *SyncResult `json:"result"`
	Error      *string     `json:"error"`
	CreatedAt  time.Time   `json:"createdAt"`
	StartedAt  *time.Time  `json:"startedAt"`
	FinishedAt *time.Time  `json:"finishedAt"`
}

type JobProgress struct {
	Documents int  `json:"documents"`
	Chunks    int  `json:"chunks"`
	Total     *int `json:"total"`
}

type Backfill struct {
	Pending int    `json:"pending"`
	State   string `json:"state"`
}

type EmbeddingConfig struct {
	Provider   string   `json:"provider"`
	Source     string   `json:"source"`
	Dimensions int      `json:"dimensions"`
	Backfill   Backfill `json:"backfill"`
}

// Scope is computed only after authenticating a request. Connection capabilities
// can access public collections; they do not impersonate a Directory group.
type Scope struct {
	Admin  bool
	Groups []string
}

func (s Scope) Visible(c Collection) bool {
	if s.Admin || len(c.Visibility.Groups) == 0 {
		return true
	}
	for _, allowed := range c.Visibility.Groups {
		for _, group := range s.Groups {
			if allowed == group {
				return true
			}
		}
	}
	return false
}
