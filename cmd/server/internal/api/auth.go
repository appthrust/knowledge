package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/appthrust/appserver-go/appserver/httpx"
	"github.com/appthrust/appserver-go/appserver/identity"
	"github.com/appthrust/knowledge/cmd/server/internal/domain"
)

func (a *API) authenticate(r *http.Request, management, mutation bool) (domain.Scope, error) {
	caller, bearer, err := a.identity.Authenticate(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		if errors.Is(err, identity.ErrUnavailable) {
			return domain.Scope{}, httpx.Fail(503, "identity_unavailable", "The identity provider is temporarily unavailable")
		}
		if errors.Is(err, identity.ErrRawKeyNotAccepted) {
			return domain.Scope{}, httpx.Fail(401, "raw_key_not_accepted", "Exchange the access key for an assertion before calling this API")
		}
		return domain.Scope{}, httpx.Fail(401, "unauthenticated", "A valid bearer token is required")
	}
	principal := caller.Principal
	if principal.Kind == "key" {
		if a.options.ProjectID == "" || principal.Project != a.options.ProjectID {
			return domain.Scope{}, httpx.Fail(401, "unauthenticated", "A valid bearer token is required")
		}
		if management {
			return domain.Scope{}, httpx.Fail(403, "key_not_permitted", "Access keys cannot administer knowledge")
		}
		required := "documents:read"
		switch {
		case r.URL.Path == "/api/knowledge/v1/search", r.URL.Path == "/api/knowledge/v1/answer":
			required = "search"
		case mutation, strings.HasPrefix(r.URL.Path, "/api/knowledge/v1/jobs/"):
			required = "documents:write"
		case r.URL.Path == "/api/knowledge/v1/openapi.json":
			// The contract describes all public operations; any declared scope
			// may discover it without gaining permission to execute them.
			if slices.ContainsFunc(principal.Scopes, func(scope string) bool {
				return scope == "search" || scope == "documents:read" || scope == "documents:write"
			}) {
				return domain.Scope{}, nil
			}
		}
		if !slices.Contains(principal.Scopes, required) {
			return domain.Scope{}, httpx.Fail(403, "insufficient_scope", "The access key does not permit this operation")
		}
		// Keys never inherit a user's groups or administrative authority.
		return domain.Scope{}, nil
	}
	if a.platform == nil {
		return domain.Scope{}, httpx.Fail(502, "upstream_unavailable", "The platform is temporarily unavailable")
	}
	access, err := a.platform.Access(r.Context(), principal.Subject, bearer)
	if err != nil {
		return domain.Scope{}, httpx.Fail(502, "upstream_unavailable", "The platform is temporarily unavailable")
	}
	// Job reads retain their existing editor/admin requirement for users.
	needsWrite := management || mutation || strings.HasPrefix(r.URL.Path, "/api/knowledge/v1/jobs/")
	if access.NotMember || (needsWrite && !access.CanCreateApps) {
		return domain.Scope{}, httpx.Forbidden()
	}
	return domain.Scope{Admin: access.IsConsoleAdmin, Groups: principal.Groups}, nil
}
