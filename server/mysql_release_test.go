// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestMySQLOnlyReleaseConfig(t *testing.T) {
	logger := zap.NewNop().WithOptions(zap.WithFatalHook(zapcore.WriteThenPanic))
	config := NewConfig(logger)
	require.Equal(t, "mysql", config.GetDatabase().Backend)
	require.NotContains(t, sql.Drivers(), "pgx", "this release must not register a PostgreSQL driver")
	require.NotContains(t, sql.Drivers(), "postgres")
	require.NotPanics(t, func() { ValidateConfigDatabase(logger, config) })
	for _, backend := range []string{"postgres", "cockroach", "", "unknown"} {
		t.Run(backend, func(t *testing.T) {
			c := NewConfig(logger)
			c.GetDatabase().Backend = backend
			require.Panics(t, func() { ValidateConfigDatabase(logger, c) })
		})
	}
	for _, address := range []string{"postgres://user:secret@localhost:5432/nakama", "postgresql://user:secret@localhost:5432/nakama"} {
		c := NewConfig(logger)
		c.GetDatabase().Addresses = []string{address}
		require.Panics(t, func() { ValidateConfigDatabase(logger, c) })
	}
}
