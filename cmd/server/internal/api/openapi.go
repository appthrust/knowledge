package api

import _ "embed"

//go:embed openapi.json
var openAPIDocument []byte

func (a *API) openAPI(s *request) (any, error) {
	s.w.Header().Set("Content-Type", "application/json")
	_, _ = s.w.Write(openAPIDocument)
	return streamed{}, nil
}
