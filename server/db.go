// Modified by nakama-mysql contributors: native MySQL backend dispatch.
// Copyright 2018 The Nakama Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"database/sql"
	"errors"
	"go.uber.org/zap"
)

var ErrDatabaseDriverMismatch = errors.New("database driver mismatch")

func DbConnect(ctx context.Context, logger *zap.Logger, config Config, create bool) *sql.DB {
	if config.GetDatabase().Backend != "mysql" {
		logger.Fatal("This release supports only database.backend=mysql")
	}
	return dbConnectMySQL(ctx, logger, config, create)
}

type Tx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	Commit() error
	Rollback() error
}
type Scannable interface {
	Scan(dest ...any) error
}

func ExecuteInTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	return executeInTxMySQL(ctx, db, fn)
}

// Arbitrary runtime SQL is executed once; uncertain outcomes are never replayed.
func ExecuteRetryable(fn func() error) error { return fn() }
