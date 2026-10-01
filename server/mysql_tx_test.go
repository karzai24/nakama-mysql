// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/stretchr/testify/require"
)

type mysqlTxProbe struct {
	commits, rollbacks     int
	commitErr, rollbackErr error
}

func (p *mysqlTxProbe) Connect(context.Context) (driver.Conn, error)                 { return p, nil }
func (p *mysqlTxProbe) Driver() driver.Driver                                        { return p }
func (p *mysqlTxProbe) Open(string) (driver.Conn, error)                             { return p, nil }
func (p *mysqlTxProbe) Prepare(string) (driver.Stmt, error)                          { return nil, errors.New("not used") }
func (p *mysqlTxProbe) Close() error                                                 { return nil }
func (p *mysqlTxProbe) Begin() (driver.Tx, error)                                    { return p, nil }
func (p *mysqlTxProbe) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) { return p, nil }
func (p *mysqlTxProbe) Commit() error                                                { p.commits++; return p.commitErr }
func (p *mysqlTxProbe) Rollback() error                                              { p.rollbacks++; return p.rollbackErr }

func TestMySQLTransactionRetrySafety(t *testing.T) {
	for _, number := range []uint16{1213, 1205} {
		t.Run((&mysql.MySQLError{Number: number}).Error(), func(t *testing.T) {
			probe := &mysqlTxProbe{}
			db := sql.OpenDB(probe)
			defer db.Close()
			calls := 0
			err := executeInTxMySQL(context.Background(), db, func(*sql.Tx) error {
				calls++
				if calls == 1 {
					return &mysql.MySQLError{Number: number}
				}
				require.Equal(t, 1, probe.rollbacks, "rollback must finish before replay")
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, 2, calls)
			require.Equal(t, 1, probe.commits)
		})
	}
	t.Run("ambiguous commit is never replayed", func(t *testing.T) {
		lost := errors.New("connection lost during commit")
		probe := &mysqlTxProbe{commitErr: lost}
		db := sql.OpenDB(probe)
		defer db.Close()
		calls := 0
		err := executeInTxMySQL(context.Background(), db, func(*sql.Tx) error { calls++; return nil })
		require.ErrorIs(t, err, lost)
		require.Equal(t, 1, calls)
	})
	t.Run("failed rollback prevents replay", func(t *testing.T) {
		probe := &mysqlTxProbe{rollbackErr: errors.New("rollback connection lost")}
		db := sql.OpenDB(probe)
		defer db.Close()
		calls := 0
		err := executeInTxMySQL(context.Background(), db, func(*sql.Tx) error { calls++; return &mysql.MySQLError{Number: 1205} })
		require.Error(t, err)
		require.Equal(t, 1, calls)
	})
	t.Run("cancellation prevents replay", func(t *testing.T) {
		probe := &mysqlTxProbe{}
		db := sql.OpenDB(probe)
		defer db.Close()
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := executeInTxMySQL(ctx, db, func(*sql.Tx) error { calls++; cancel(); return &mysql.MySQLError{Number: 1213} })
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 1, calls)
	})
}

func TestDualBackendTransactionRetryExhaustion(t *testing.T) {
	for _, backend := range []string{"mysql"} {
		t.Run(backend, func(t *testing.T) {
			retry := error(&mysql.MySQLError{Number: 1213})
			runner := executeInTxMySQL

			probe := &mysqlTxProbe{}
			db := sql.OpenDB(probe)
			defer db.Close()
			calls := 0
			err := runner(context.Background(), db, func(*sql.Tx) error { calls++; return retry })
			require.ErrorIs(t, err, retry, "exhausted retries must never report success")
			require.Equal(t, 5, calls)
			require.Equal(t, 5, probe.rollbacks)
			require.Zero(t, probe.commits)
		})
	}
}
