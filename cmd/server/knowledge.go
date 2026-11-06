package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/appthrust/appserver-go/apphealth"
	"github.com/appthrust/appserver-go/appserver/health"
	"github.com/appthrust/appserver-go/appserver/identity"
	"github.com/appthrust/appserver-go/appserver/pending"
	"github.com/appthrust/appserver-go/appserver/platformclient"
	"github.com/appthrust/appserver-go/appserver/releaseidentity"
	"github.com/appthrust/knowledge/cmd/server/internal/api"
	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/appthrust/knowledge/cmd/server/internal/store"
)

const storeOpenTimeout = 30 * time.Second

var storeRetryInitial = time.Second

type knowledgeRuntime struct {
	*pending.Handler
	embedding *domain.Embedder
	database  atomic.Pointer[store.Store]
	health    health.Registry
}

func (k *knowledgeRuntime) EmbeddingConfig(ctx context.Context) (domain.EmbeddingConfig, error) {
	return api.EmbeddingConfig(ctx, k.database.Load(), k.embedding)
}

func (k *knowledgeRuntime) HealthHandler() http.Handler {
	return k.health.Handler(releaseidentity.HealthIdentity("KNOWLEDGE"))
}

type workerStore struct {
	*store.Store
	stop func()
}

func (s *workerStore) Close() error {
	s.stop()
	return s.Store.Close()
}
func runtimeOptions(projectID string) (api.Options, error) {
	options := api.Options{ProjectID: projectID, NotionToken: os.Getenv("KNOWLEDGE_NOTION_TOKEN"), EsaToken: os.Getenv("KNOWLEDGE_ESA_TOKEN")}
	var err error
	options.Embedding, err = domain.EmbeddingFromEnv()
	if err != nil {
		return options, err
	}
	options.LLM, err = domain.LLMFromEnv()
	if err != nil {
		return options, err
	}
	notionURL, err := domain.ProviderURL(os.Getenv("KNOWLEDGE_NOTION_BASE_URL"), "https://api.notion.com")
	if err != nil {
		return options, err
	}
	options.Notion = &domain.Notion{BaseURL: notionURL, Client: domain.HTTPClient()}
	esaURL, err := domain.ProviderURL(os.Getenv("KNOWLEDGE_ESA_BASE_URL"), "https://api.esa.io")
	if err != nil {
		return options, err
	}
	options.Esa = &domain.Esa{BaseURL: esaURL, Client: domain.HTTPClient()}
	return options, nil
}
func knowledgeHandler(upstreamURL, projectID string) (*knowledgeRuntime, func(), error) {
	auth, err := identity.New(identity.FromEnv("KNOWLEDGE"))
	if err != nil {
		return nil, nil, err
	}
	options, err := runtimeOptions(projectID)
	if err != nil {
		return nil, nil, err
	}
	publicURL := os.Getenv("KNOWLEDGE_PUBLIC_URL")
	if publicURL != "" {
		origin, err := parseOrigin(publicURL)
		if err != nil || origin.Scheme != "https" || origin.Path != "" || strings.Contains(publicURL, "#") {
			return nil, nil, errors.New("KNOWLEDGE_PUBLIC_URL must be an HTTPS origin without a path, query, or fragment")
		}
	}
	knowledge := &knowledgeRuntime{Handler: pending.Serve(api.Unavailable()), embedding: options.Embedding}
	options.Health = &api.HealthEvidence{}
	databaseHealth := health.RateLimited(health.CheckFunc(func(ctx context.Context) health.Status {
		database := knowledge.database.Load()
		if database == nil {
			return health.Status{State: apphealth.StateFailing, Reason: apphealth.ReasonDependencyUnavailable, Message: "The knowledge database is unavailable"}
		}
		err := database.Do(ctx, false, func(tx *store.Tx) error { return tx.Exec("SELECT 1") })
		if err != nil {
			return health.Status{State: apphealth.StateFailing, Reason: apphealth.ReasonDependencyUnavailable, Message: "The knowledge database ping failed"}
		}
		return health.Status{State: apphealth.StateServing}
	}))
	knowledge.health.Capability("search", true, health.WithDependency(&options.Health.Search, databaseHealth))
	knowledge.health.Capability("ingest", true, health.WithDependency(&options.Health.Ingest, databaseHealth))
	gatewayHealth := health.RateLimited(health.CheckFunc(func(ctx context.Context) health.Status {
		if options.LLM == nil {
			return health.Status{State: apphealth.StateFailing, Reason: apphealth.ReasonDependencyUnavailable, Message: "No AI gateway is configured"}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, options.LLM.BaseURL+"/models", nil)
		if err != nil {
			return health.Status{State: apphealth.StateUnknown, Reason: apphealth.ReasonNoEvidence}
		}
		response, err := options.LLM.Client.Do(request)
		if err != nil {
			return health.Status{State: apphealth.StateDegraded, Reason: apphealth.ReasonUpstreamUnavailable, Message: "The AI gateway could not be reached"}
		}
		defer response.Body.Close()
		// No machine credential exists. A 401 establishes reachability only;
		// never retain a caller bearer or infer that Ask can serve from this.
		if response.StatusCode == http.StatusOK || response.StatusCode == http.StatusUnauthorized {
			return health.Status{State: apphealth.StateUnknown, Reason: apphealth.ReasonNoEvidence, Message: "The AI gateway is reachable; no recent Ask outcome"}
		}
		if response.StatusCode == http.StatusForbidden {
			return health.Status{State: apphealth.StateDegraded, Reason: apphealth.ReasonUpstreamDenied, Message: "The AI gateway denied model discovery"}
		}
		return health.Status{State: apphealth.StateDegraded, Reason: apphealth.ReasonUpstreamUnavailable, Message: "The AI gateway model endpoint is unavailable"}
	}))
	knowledge.health.Capability("ask", false, health.CheckFunc(func(ctx context.Context) health.Status {
		dependency := gatewayHealth.Check(ctx)
		if dependency.State != apphealth.StateUnknown {
			return dependency
		}
		outcome := options.Health.Ask.Check(ctx)
		if outcome.State == apphealth.StateUnknown {
			return dependency
		}
		return outcome
	}))
	dsn := os.Getenv("KNOWLEDGE_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		knowledge.Swap(api.Unavailable())
		return knowledge, func() {}, nil
	}
	seed := os.Getenv("KNOWLEDGE_DEV_SEED") == "1"
	stop := pending.Connect("knowledge database", storeRetryInitial, func(ctx context.Context) (io.Closer, error) {
		ctx, cancel := context.WithTimeout(ctx, storeOpenTimeout)
		defer cancel()
		database, err := store.Open(ctx, dsn)
		if err != nil {
			return nil, err
		}
		if seed {
			if err := database.SeedDev(ctx, options.Embedding); err != nil {
				_ = database.Close()
				return nil, err
			}
		}
		stopWorker, err := api.StartWorker(database, options)
		if err != nil {
			_ = database.Close()
			return nil, err
		}
		knowledge.database.Store(database)
		knowledge.Swap(api.New(database, auth, platformclient.New(upstreamURL, projectID, platformclient.WithHTTPClient(auth.HTTPClient())), options))
		return &workerStore{Store: database, stop: stopWorker}, nil
	})
	return knowledge, stop, nil
}
