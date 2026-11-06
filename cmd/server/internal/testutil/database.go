// Package testutil provides the built-in app's opt-in pgvector test database.
package testutil

import (
	"context"
	"net/url"
	"testing"

	pgtest "github.com/appthrust/appserver-go/appserver/pgstore/testutil"
	"github.com/appthrust/knowledge/cmd/server/internal/store"
)

func Database(t *testing.T) (*store.Store, string) {
	var actual string
	db, _ := pgtest.Database(t, func(ctx context.Context, dsn string) (*store.Store, error) {
		u, err := url.Parse(dsn)
		if err != nil {
			return nil, err
		}
		q := u.Query()
		q.Set("search_path", q.Get("search_path")+",public")
		u.RawQuery = q.Encode()
		actual = u.String()
		return store.Open(ctx, actual)
	})
	return db, actual
}
