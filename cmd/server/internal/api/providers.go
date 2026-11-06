package api

import (
	"sync"
	"time"

	"github.com/appthrust/knowledge/cmd/server/internal/domain"
)

// esaConnectionTTL bounds probe traffic against esa's per-user rate limit
// while the Sources page polls connection state.
const esaConnectionTTL = time.Minute

type EsaConnection struct {
	PlatformManaged bool             `json:"platformManaged"`
	Connected       bool             `json:"connected"`
	Teams           []domain.EsaTeam `json:"teams"`
	Error           string           `json:"error,omitempty"`
}

type esaConnectionCache struct {
	mu      sync.Mutex
	token   string
	value   EsaConnection
	expires time.Time
}

// esaConnection probes esa with the platform-managed token. Stored-mode
// sources carry their own tokens and are never probed here.
func (a *API) esaConnection(s *request) (any, error) {
	token := a.options.EsaToken
	if token == "" {
		return EsaConnection{Teams: []domain.EsaTeam{}}, nil
	}
	c := &a.esa
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token && a.now().Before(c.expires) {
		return c.value, nil
	}
	value := EsaConnection{PlatformManaged: true, Teams: []domain.EsaTeam{}}
	teams, err := a.options.Esa.Teams(s.r.Context(), token)
	if err != nil {
		value.Error = err.Error()
	} else {
		value.Connected = true
		value.Teams = teams
	}
	c.token, c.value, c.expires = token, value, a.now().Add(esaConnectionTTL)
	return value, nil
}
