package api

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/appthrust/appserver-go/apphealth"
	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/appthrust/knowledge/cmd/server/internal/store"
)

func (a *API) job(s *request) (any, error) {
	var job domain.IngestJob
	err := a.store.Do(s.r.Context(), false, func(tx *store.Tx) error {
		var err error
		job, err = tx.Job(s.r.PathValue("id"))
		if err != nil || job.SourceID == "" {
			return err
		}
		source, err := tx.Source(job.SourceID)
		if err != nil {
			return err
		}
		_, err = tx.Collection(source.Collection, s.scope)
		return err
	})
	return job, err
}

// EmbeddingConfig reads live backfill state, not a startup snapshot.
func (a *API) EmbeddingConfig(ctx context.Context) (domain.EmbeddingConfig, error) {
	return EmbeddingConfig(ctx, a.store, a.options.Embedding)
}

func EmbeddingConfig(ctx context.Context, database *store.Store, e *domain.Embedder) (domain.EmbeddingConfig, error) {
	out := domain.EmbeddingConfig{Provider: e.Provider, Source: e.Source, Dimensions: e.DimensionCount(), Backfill: domain.Backfill{State: "idle"}}
	if database == nil {
		return out, nil
	}
	err := database.Do(ctx, false, func(tx *store.Tx) error {
		var err error
		out.Backfill, err = tx.Backfill(e.IndexProvider())
		return err
	})
	return out, err
}

// StartWorker starts the process's single FIFO consumer. The owner must stop it
// before closing the store. Cancellation leaves the checkpointed job running;
// store.Open requeues it after either a graceful stop or a process crash.
func StartWorker(database *store.Store, options Options) (func(), error) {
	ctx, cancel := context.WithCancel(context.Background())
	if err := database.Do(ctx, true, func(tx *store.Tx) error { return tx.EnsureReembed(options.Embedding.IndexProvider()) }); err != nil {
		cancel()
		return nil, err
	}
	done := make(chan struct{})
	worker := &API{store: database, options: options}
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for ctx.Err() == nil {
			var job *domain.IngestJob
			err := database.Do(ctx, true, func(tx *store.Tx) error {
				var err error
				job, err = tx.ClaimJob()
				return err
			})
			if err == nil && job != nil {
				var result *domain.SyncResult
				if job.Kind == "sync" {
					result, err = worker.syncJob(ctx, job)
				} else {
					err = worker.reembedJob(ctx, job)
				}
				if ctx.Err() != nil {
					return
				}
				failure := err
				err = database.Do(ctx, true, func(tx *store.Tx) error { return tx.FinishJob(*job, result, failure) })
				if options.Health != nil {
					ok := failure == nil && err == nil && (result == nil || len(result.Errors) == 0)
					options.Health.Ingest.Record(ok, apphealth.ReasonDependencyUnavailable)
				}
				if err == nil {
					continue
				}
				// A failed final write must not strand the job until a restart.
				// Retry that write, not the ingestion which already checkpointed.
				for err != nil && ctx.Err() == nil {
					log.Printf("knowledge job completion unavailable: %v", err)
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
					}
					err = database.Do(ctx, true, func(tx *store.Tx) error { return tx.FinishJob(*job, result, failure) })
				}
				continue
			}
			if err != nil && ctx.Err() == nil {
				log.Printf("knowledge job queue unavailable: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }, nil
}

func (a *API) syncJob(ctx context.Context, job *domain.IngestJob) (*domain.SyncResult, error) {
	var source domain.Source
	err := a.store.Do(ctx, true, func(tx *store.Tx) error {
		var err error
		source, err = tx.Source(job.SourceID)
		if err != nil {
			return err
		}
		return tx.EnsureReembed(a.options.Embedding.IndexProvider())
	})
	if err != nil {
		return nil, err
	}
	if token := a.options.managedToken(source.Type); token != "" {
		source.Config.Token = token
	}
	var walk func(context.Context, domain.SourceConfig, func(string, string) (bool, error), func(domain.Document, string) error) ([]string, error)
	switch source.Type {
	case "notion":
		walk = a.options.Notion.Sync
	case "esa":
		walk = a.options.Esa.Sync
	default:
		return nil, errors.New("unsupported source type")
	}
	result := &domain.SyncResult{Documents: job.Progress.Documents, Chunks: job.Progress.Chunks, Errors: []string{}}
	var progressErr error
	// Every page or post the walk reaches is checked for changes first, so the
	// checked IDs are the source's current snapshot.
	var seen []string
	result.Errors, err = walk(ctx, source.Config, func(id, edited string) (bool, error) {
		seen = append(seen, id)
		var same bool
		err := a.store.Do(ctx, false, func(tx *store.Tx) error {
			var err error
			same, err = tx.Unchanged(source.ID, id, edited)
			return err
		})
		return same, err
	}, func(d domain.Document, edited string) error {
		if progressErr != nil {
			return progressErr
		}
		d.Collection = source.Collection
		pageID := d.ExternalID
		d.ExternalID = source.Type + ":" + source.ID + ":" + pageID
		chunks, err := a.options.Embedding.Chunks(ctx, d.Title, d.Content)
		if err != nil {
			return errors.New("the embedding provider is unavailable")
		}
		next := job.Progress
		next.Documents++
		next.Chunks += len(chunks)
		err = a.store.Do(ctx, true, func(tx *store.Tx) error {
			var err error
			d, err = tx.PutDocument(d, chunks, a.options.Embedding.IndexProvider())
			if err != nil {
				return err
			}
			if err = tx.Revision(source.ID, pageID, edited, d.ID); err != nil {
				return err
			}
			progressErr = tx.JobProgress(job.ID, next)
			return progressErr
		})
		if err == nil {
			job.Progress = next
			result.Documents, result.Chunks = next.Documents, next.Chunks
		}
		return err
	})
	// Only a complete, error-free walk is a trustworthy snapshot: a truncated
	// or partially failed one must not delete what it failed to reach.
	if err == nil && progressErr == nil && len(result.Errors) == 0 {
		err = a.store.Do(ctx, true, func(tx *store.Tx) error {
			var err error
			result.Removed, err = tx.Prune(source.ID, seen)
			return err
		})
	}
	result.FinishedAt = time.Now().UTC()
	return result, errors.Join(err, progressErr)
}

func (a *API) reembedJob(ctx context.Context, job *domain.IngestJob) error {
	provider := a.options.Embedding.IndexProvider()
	for ctx.Err() == nil {
		var docs []domain.Document
		err := a.store.Do(ctx, false, func(tx *store.Tx) error {
			var err error
			docs, err = tx.ReembedCandidates(provider)
			return err
		})
		if err != nil || len(docs) == 0 {
			return err
		}
		for _, d := range docs {
			chunks, err := a.options.Embedding.Chunks(ctx, d.Title, d.Content)
			if err != nil {
				return err
			}
			err = a.store.Do(ctx, true, func(tx *store.Tx) error {
				replaced, err := tx.ReplaceEmbeddings(d, chunks, provider)
				if err != nil || !replaced {
					return err
				}
				job.Progress.Documents++
				job.Progress.Chunks += len(chunks)
				pending, err := tx.ReembedPending(provider)
				if err != nil {
					return err
				}
				total := job.Progress.Documents + pending
				job.Progress.Total = &total
				return tx.JobProgress(job.ID, job.Progress)
			})
			if err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}
