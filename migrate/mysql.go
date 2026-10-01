// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

//go:embed mysql/*.sql
var mysqlMigrations embed.FS

const mysqlBaseline = "0001_nakama_340"

var mysqlVersions = []string{mysqlBaseline, "0002_stable_group_primary_key"}

func mysqlMigrationChecksum(version ...string) string {
	id := mysqlBaseline
	if len(version) > 0 {
		id = version[0]
	}
	up, _ := mysqlMigrations.ReadFile("mysql/" + id + ".up.sql")
	down, _ := mysqlMigrations.ReadFile("mysql/" + id + ".down.sql")
	return fmt.Sprintf("%x", sha256.Sum256(append(up, down...)))
}

type mysqlMigrationReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// Applied versions must be an unmodified, clean prefix of the known history.
func mysqlApplied(ctx context.Context, db mysqlMigrationReader) (int, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, checksum, dirty FROM migration_info_mysql ORDER BY id")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id, checksum string
		var dirty bool
		if err = rows.Scan(&id, &checksum, &dirty); err != nil {
			return 0, err
		}
		if count >= len(mysqlVersions) || id != mysqlVersions[count] || checksum != mysqlMigrationChecksum(id) || dirty {
			return 0, errors.New("MySQL migration is unknown, modified, missing, or dirty; manual inspection required")
		}
		count++
	}
	return count, rows.Err()
}

// CheckMySQL fails closed on partial DDL, unknown schemas, or changed migrations.
func CheckMySQL(ctx context.Context, db *sql.DB) error {
	count, err := mysqlApplied(ctx, db)
	if err != nil {
		return fmt.Errorf("MySQL schema unavailable or unsafe: %w", err)
	}
	if count != len(mysqlVersions) {
		return errors.New("MySQL schema version mismatch; run migrate up")
	}
	return nil
}

func RunMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, cmd string, limit int) error {
	if cmd != "up" && cmd != "down" && cmd != "redo" && cmd != "status" {
		return errors.New("MySQL migrate requires up, down, redo, or status")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var dbName string
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&dbName); err != nil {
		return err
	}
	lock := fmt.Sprintf("nakama-migrate:%x", sha256.Sum256([]byte(dbName)))[:64]
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 30)", lock).Scan(&acquired); err != nil {
		return err
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return errors.New("could not acquire MySQL migration lock")
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(releaseCtx, "DO RELEASE_LOCK(?)", lock)
	}()
	_, err = conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS migration_info_mysql (
        id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
        checksum CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
        dirty BOOLEAN NOT NULL,
        applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
    ) ENGINE=InnoDB`)
	if err != nil {
		return err
	}

	present, err := mysqlApplied(ctx, conn)
	if err != nil {
		return err
	}
	if cmd == "status" {
		logger.Info("MySQL schema", zap.Int("applied", present), zap.Int("available", len(mysqlVersions)), zap.Bool("current", present == len(mysqlVersions)))
		return nil
	}
	apply := func(id, direction string) error {
		content, err := mysqlMigrations.ReadFile("mysql/" + id + "." + direction + ".sql")
		if err != nil {
			return err
		}
		if direction == "up" {
			if id == mysqlBaseline {
				var count int
				if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name <> 'migration_info_mysql'").Scan(&count); err != nil {
					return err
				}
				if count != 0 {
					return errors.New("initial MySQL migration requires an empty database")
				}
				var binaryLog, trustCreators bool
				if err = conn.QueryRowContext(ctx, "SELECT @@GLOBAL.log_bin, @@GLOBAL.log_bin_trust_function_creators").Scan(&binaryLog, &trustCreators); err != nil {
					return err
				}
				if binaryLog && !trustCreators {
					return errors.New("MySQL migrations require log_bin_trust_function_creators=ON when binary logging is enabled; no schema changes applied")
				}
			}
			_, err = conn.ExecContext(ctx, "INSERT INTO migration_info_mysql (id,checksum,dirty) VALUES (?,?,true)", id, mysqlMigrationChecksum(id))
		} else {
			_, err = conn.ExecContext(ctx, "UPDATE migration_info_mysql SET dirty = true WHERE id = ?", id)
		}
		if err != nil {
			return err
		}
		for i, statement := range strings.Split(string(content), "-- statement-break") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err = conn.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("MySQL migration %s %s statement %d failed (schema left dirty): %w", id, direction, i+1, err)
			}
		}
		if direction == "up" {
			_, err = conn.ExecContext(ctx, "UPDATE migration_info_mysql SET dirty = false, applied_at = CURRENT_TIMESTAMP(6) WHERE id = ?", id)
		} else {
			_, err = conn.ExecContext(ctx, "DELETE FROM migration_info_mysql WHERE id = ?", id)
		}
		return err
	}
	// Nonpositive limits apply all pending/installed versions, matching the CLI.
	steps := func(available int) int {
		if limit > 0 && limit < available {
			return limit
		}
		return available
	}
	redoCount := 0
	if cmd == "down" || cmd == "redo" {
		n := steps(present)
		redoCount = n
		for i := 0; i < n; i++ {
			if err = apply(mysqlVersions[present-1], "down"); err != nil {
				return err
			}
			present--
		}
	}
	if cmd == "up" || cmd == "redo" {
		n := steps(len(mysqlVersions) - present)
		if cmd == "redo" && redoCount > 0 {
			n = redoCount
		}
		for i := 0; i < n; i++ {
			if err = apply(mysqlVersions[present], "up"); err != nil {
				return err
			}
			present++
		}
	}
	logger.Info("MySQL migration complete", zap.String("command", cmd), zap.Int("applied", present))
	return nil
}
