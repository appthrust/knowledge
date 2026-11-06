package store

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/appthrust/appserver-go/appserver/pgstore"
	"github.com/appthrust/knowledge/cmd/server/internal/domain"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestDocumentsBoundsOnlyVisibleCollections(t *testing.T) {
	for _, tc := range []struct {
		name, collection string
		scope            domain.Scope
		names            []string
		limit            int
	}{
		{"connection", "", domain.Scope{}, []string{"manual", "other"}, 100},
		{"filter", "manual", domain.Scope{}, []string{"manual"}, 1},
		{"unknown", "missing", domain.Scope{}, []string{}, 500},
		{"hidden", "secret", domain.Scope{}, []string{}, 1},
		{"group member", "secret", domain.Scope{Groups: []string{"engineering"}}, []string{"secret"}, 1},
		{"admin", "", domain.Scope{Admin: true}, []string{"manual", "other", "secret"}, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectBegin()
			sqlTx, err := db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			tx := &Tx{&pgstore.Tx{SQL: sqlTx, Context: context.Background()}}
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT name,description,visibility_groups FROM collections ORDER BY name`)).WillReturnRows(
				sqlmock.NewRows([]string{"name", "description", "visibility_groups"}).
					AddRow("manual", "", "{}").AddRow("other", "", "{}").AddRow("secret", "", "{engineering}"))
			rows := sqlmock.NewRows([]string{"id", "collection", "external_id", "title", "url", "content", "metadata", "chunks"})
			if len(tc.names) > 0 {
				rows.AddRow("doc", tc.names[0], "", "Handbook", "", "Content", []byte(`{}`), 0)
			}
			mock.ExpectQuery(regexp.QuoteMeta(documentSelect+` WHERE d.collection=ANY($1) ORDER BY d.updated_at DESC,d.id LIMIT $2`)).
				WithArgs(pq.Array(tc.names), tc.limit).WillReturnRows(rows)
			documents, err := tx.Documents(tc.collection, tc.limit, tc.scope)
			require.NoError(t, err)
			require.NotNil(t, documents)
			if len(tc.names) == 0 {
				require.Empty(t, documents)
			} else {
				require.Len(t, documents, 1)
				require.Equal(t, tc.names[0], documents[0].Collection)
			}
			mock.ExpectRollback()
			require.NoError(t, sqlTx.Rollback())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
