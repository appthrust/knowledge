package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/appthrust/appserver-go/apphealth"
	"github.com/appthrust/appserver-go/appserver/httpx"
	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestAskHealthUsesLLMFailureAndStreamOutcome(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   any
		err    error
		reason string
	}{
		{"gateway 502", nil, llmFailure(errors.New("upstream URL with secret"), "knowledge_llm_error"), apphealth.ReasonUpstreamUnavailable},
		{"policy denial", nil, llmFailure(&domain.LLMError{StatusCode: http.StatusForbidden}, "knowledge_llm_error"), apphealth.ReasonUpstreamDenied},
		{"stream interrupted", streamed{failed: true}, nil, apphealth.ReasonUpstreamUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &API{options: Options{Health: &HealthEvidence{}}}
			for range 3 {
				a.recordHealth("POST /api/knowledge/v1/answer", tc.body, tc.err)
			}
			status := a.options.Health.Ask.Check(context.Background())
			require.Equal(t, apphealth.StateFailing, status.State)
			require.Equal(t, tc.reason, status.Reason)
			require.NotContains(t, status.Message, "secret")
			require.Equal(t, apphealth.StateUnknown, a.options.Health.Search.Check(context.Background()).State)
		})
	}
}

func TestHealthIgnoresInvalidInputAndTracksSearchAndIngest(t *testing.T) {
	a := &API{options: Options{Health: &HealthEvidence{}}}
	a.recordHealth("POST /api/knowledge/v1/answer", nil, httpx.Invalid("bad query"))
	require.Equal(t, apphealth.StateUnknown, a.options.Health.Ask.Check(context.Background()).State)
	a.recordHealth("POST /api/knowledge/v1/search", map[string]any{"hits": []any{}}, nil)
	a.recordHealth("POST /api/knowledge/v1/documents", nil, nil)
	require.Equal(t, apphealth.StateServing, a.options.Health.Search.Check(context.Background()).State)
	require.Equal(t, apphealth.StateServing, a.options.Health.Ingest.Check(context.Background()).State)
	a.recordHealth("POST /api/knowledge/v1/answer", streamed{}, nil)
	require.Equal(t, apphealth.StateServing, a.options.Health.Ask.Check(context.Background()).State)
}
