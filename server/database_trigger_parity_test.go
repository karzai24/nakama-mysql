// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"database/sql"
	"github.com/gofrs/uuid/v5"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func failureTrigger(t *testing.T, db *sql.DB, table, event, predicate string) func() {
	t.Helper()
	name := "parity_fail_" + strings.ReplaceAll(uuid.Must(uuid.NewV4()).String(), "-", "")
	var statement string
	{
		statement = "CREATE TRIGGER " + name + " BEFORE " + event + " ON " + table + " FOR EACH ROW BEGIN IF " + predicate + " THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='parity rollback fixture'; END IF; END"
	}

	_, err := db.Exec(statement)
	require.NoError(t, err)
	drop := func() {
		statement := "DROP TRIGGER IF EXISTS " + name

		_, err := db.Exec(statement)
		require.NoError(t, err)

	}
	t.Cleanup(drop)
	return drop
}
