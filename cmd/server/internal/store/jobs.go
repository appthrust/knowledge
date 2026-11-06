package store

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/google/uuid"
)

const jobColumns = `id,kind,COALESCE(source_id,''),state,progress,result,error,created_at,started_at,finished_at`

func scanJob(row interface{ Scan(...any) error }) (domain.IngestJob, error) {
	var job domain.IngestJob
	var progress, result []byte
	err := row.Scan(&job.ID, &job.Kind, &job.SourceID, &job.State, &progress, &result, &job.Error, &job.CreatedAt, &job.StartedAt, &job.FinishedAt)
	if err != nil {
		return job, err
	}
	if err = json.Unmarshal(progress, &job.Progress); err != nil {
		return job, err
	}
	if len(result) > 0 {
		err = json.Unmarshal(result, &job.Result)
	}
	return job, err
}

func (t *Tx) Job(id string) (domain.IngestJob, error) {
	return scanJob(t.SQL.QueryRowContext(t.Context, `SELECT `+jobColumns+` FROM ingest_jobs WHERE id=$1`, id))
}

func (t *Tx) LatestSync(sourceID string) (*domain.IngestJob, error) {
	job, err := scanJob(t.SQL.QueryRowContext(t.Context, `SELECT `+jobColumns+` FROM ingest_jobs WHERE source_id=$1 AND kind='sync' ORDER BY created_at DESC,id DESC LIMIT 1`, sourceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &job, err
}

// EnqueueSync is called in a write transaction; the shared writer lock makes
// the lookup and insert atomic even for simultaneous HTTP requests.
func (t *Tx) EnqueueSync(sourceID string) (domain.IngestJob, error) {
	job, err := scanJob(t.SQL.QueryRowContext(t.Context, `SELECT `+jobColumns+` FROM ingest_jobs WHERE source_id=$1 AND kind='sync' AND state IN ('queued','running')`, sourceID))
	if !errors.Is(err, sql.ErrNoRows) {
		return job, err
	}
	return scanJob(t.SQL.QueryRowContext(t.Context, `INSERT INTO ingest_jobs(id,kind,source_id,state) VALUES($1,'sync',$2,'queued') RETURNING `+jobColumns, uuid.NewString(), sourceID))
}

func (t *Tx) EnsureReembed(provider string) error {
	pending, err := t.ReembedPending(provider)
	if err != nil || pending == 0 {
		return err
	}
	progress, err := json.Marshal(domain.JobProgress{Total: &pending})
	if err != nil {
		return err
	}
	return t.Exec(`INSERT INTO ingest_jobs(id,kind,state,progress) VALUES($1,'reembed','queued',$2) ON CONFLICT DO NOTHING`, uuid.NewString(), string(progress))
}

func (t *Tx) ClaimJob() (*domain.IngestJob, error) {
	job, err := scanJob(t.SQL.QueryRowContext(t.Context, `UPDATE ingest_jobs SET state='running',started_at=COALESCE(started_at,now()) WHERE id=(SELECT id FROM ingest_jobs WHERE state='queued' ORDER BY created_at,id LIMIT 1 FOR UPDATE SKIP LOCKED) RETURNING `+jobColumns))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &job, err
}

func (t *Tx) JobProgress(id string, progress domain.JobProgress) error {
	body, err := json.Marshal(progress)
	if err != nil {
		return err
	}
	return t.Exec(`UPDATE ingest_jobs SET progress=$2 WHERE id=$1`, id, string(body))
}

func (t *Tx) FinishJob(job domain.IngestJob, result *domain.SyncResult, failure error) error {
	state := "done"
	var message *string
	if failure != nil {
		state = "failed"
		text := failure.Error()
		message = &text
	}
	var body any
	if result != nil {
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		body = string(encoded)
	}
	if err := t.Exec(`UPDATE ingest_jobs SET state=$2,result=$3,error=$4,finished_at=now() WHERE id=$1`, job.ID, state, body, message); err != nil {
		return err
	}
	if job.Kind == "sync" && failure == nil && result != nil {
		return t.SyncResult(job.SourceID, *result)
	}
	return nil
}

func (t *Tx) Backfill(provider string) (domain.Backfill, error) {
	out := domain.Backfill{State: "idle"}
	var err error
	out.Pending, err = t.ReembedPending(provider)
	if err != nil {
		return out, err
	}
	var state string
	err = t.SQL.QueryRowContext(t.Context, `SELECT state FROM ingest_jobs WHERE kind='reembed' ORDER BY (state IN ('queued','running')) DESC,created_at DESC,id DESC LIMIT 1`).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if state == "queued" || state == "running" || (state == "failed" && out.Pending > 0) {
		out.State = state
	}
	return out, err
}
