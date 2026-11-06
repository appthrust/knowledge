package api

import (
	"errors"

	"github.com/appthrust/appserver-go/apphealth"
	"github.com/appthrust/appserver-go/appserver/health"
	"github.com/appthrust/appserver-go/appserver/httpx"
)

// HealthEvidence is shared by the HTTP API and the ingestion worker. It stores
// only bounded outcome classes, never prompts, documents, URLs or credentials.
type HealthEvidence struct {
	Search health.Outcomes
	Ask    health.Outcomes
	Ingest health.Outcomes
}

func (a *API) recordHealth(pattern string, body any, err error) {
	if a.options.Health == nil {
		return
	}
	var outcomes *health.Outcomes
	switch pattern {
	case "GET /api/knowledge/v1/search", "POST /api/knowledge/v1/search":
		outcomes = &a.options.Health.Search
	case "POST /api/knowledge/v1/answer":
		outcomes = &a.options.Health.Ask
	case "POST /api/knowledge/v1/documents":
		outcomes = &a.options.Health.Ingest
	default:
		return
	}
	class := apphealth.ReasonDependencyUnavailable
	var failure *httpx.Error
	if errors.As(err, &failure) {
		switch {
		case failure.Code == "knowledge_llm_denied":
			class = apphealth.ReasonUpstreamDenied
		case failure.Status < 500:
			return // Invalid input and caller permissions are not application failures.
		case failure.Status == 502 || failure.Code == "knowledge_llm_unavailable":
			class = apphealth.ReasonUpstreamUnavailable
		}
	}
	if stream, ok := body.(streamed); ok && stream.failed {
		outcomes.Record(false, apphealth.ReasonUpstreamUnavailable)
		return
	}
	outcomes.Record(err == nil, class)
}
