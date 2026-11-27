package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestKeyRevokedSeparatesLookupErrors(t *testing.T) {
	db, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := principal{agent: "alice", keyHash: "hash"}

	db.ExpectQuery("SELECT EXISTS").WithArgs("hash").WillReturnError(errors.New("database unavailable"))
	if revoked, err := keyRevoked(context.Background(), db, p); err == nil || revoked {
		t.Fatalf("lookup failure = (%v, %v), want an error and not revoked", revoked, err)
	}
	db.ExpectQuery("SELECT EXISTS").WithArgs("hash").WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
	if revoked, err := keyRevoked(context.Background(), db, p); err != nil || !revoked {
		t.Fatalf("deleted key = (%v, %v), want revoked", revoked, err)
	}
	if revoked, err := keyRevoked(context.Background(), db, principal{admin: true}); err != nil || revoked {
		t.Fatalf("admin principal = (%v, %v), want not revoked", revoked, err)
	}
	if err := db.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
