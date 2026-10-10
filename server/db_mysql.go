// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
)

// DbIsMySQL is connection-scoped: tests and tools may open both backends.
func DbIsMySQL(db *sql.DB) bool {
	_, ok := db.Driver().(*mysql.MySQLDriver)
	return ok
}

// DbMySQLConfig uses the same URL-style address as PostgreSQL, not a driver DSN.
// Only TLS is configurable in the URL. Correctness-critical session options
// cannot be overridden (UTC, strict SQL, JSON charset, and prepared statements).
func DbMySQLConfig(config Config) (*mysql.Config, error) {
	if len(config.GetDatabase().Addresses) != 1 {
		return nil, errors.New("MySQL requires exactly one database address")
	}
	address := config.GetDatabase().Addresses[0]
	if !strings.Contains(address, "://") {
		address = "mysql://" + address
	}
	u, err := url.Parse(address)
	if err != nil {
		return nil, errors.New("invalid MySQL connection URL")
	}
	if u.Scheme != "mysql" || u.Hostname() == "" || u.User == nil || u.User.Username() == "" || u.Fragment != "" {
		return nil, errors.New("MySQL address must be user:password@host:port/database?tls=true")
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" || strings.Contains(name, "/") || len(name) > 64 {
		return nil, errors.New("MySQL database name must contain 1-64 bytes and no slash")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("invalid MySQL connection parameters")
	}
	for k, values := range q {
		if k != "tls" || len(values) != 1 {
			return nil, errors.New("MySQL address only accepts a single tls parameter")
		}
	}
	tls := q.Get("tls")
	if tls == "" {
		tls = "true"
	}
	if tls != "true" && tls != "false" {
		return nil, errors.New("MySQL tls must be true (verified TLS) or false (local development)")
	}
	port := u.Port()
	if port == "" {
		port = "3306"
	}
	c := mysql.NewConfig()
	c.User, c.Passwd = u.User.Username(), ""
	c.Passwd, _ = u.User.Password()
	c.Net, c.Addr, c.DBName = "tcp", net.JoinHostPort(u.Hostname(), port), name
	c.TLSConfig = tls
	c.ParseTime, c.Loc = true, time.UTC
	if err := c.Apply(mysql.Charset("utf8mb4", "utf8mb4_bin")); err != nil {
		return nil, err
	}
	c.Timeout = 15 * time.Second
	c.ReadTimeout, c.WriteTimeout = 30*time.Second, 30*time.Second
	c.ClientFoundRows = true
	c.Params = map[string]string{
		"time_zone": "'+00:00'",
		"sql_mode":  "'STRICT_ALL_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'",
	}
	return c, nil
}

func dbConnectMySQL(ctx context.Context, logger *zap.Logger, config Config, create bool) *sql.DB {
	c, err := DbMySQLConfig(config)
	if err != nil {
		logger.Fatal("Bad MySQL configuration", zap.Error(err))
	}
	db, err := openMySQL(c)
	if err != nil {
		logger.Fatal("Failed to configure MySQL connector", zap.Error(err))
	}
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err = db.PingContext(pingCtx)
	if create && mysqlErrorNumber(err, 1049) {
		db.Close()
		adminConfig := c.Clone()
		adminConfig.DBName = ""
		admin, e := openMySQL(adminConfig)
		if e != nil {
			logger.Fatal("Failed to configure database creation", zap.Error(e))
		}
		_, e = admin.ExecContext(pingCtx, "CREATE DATABASE `"+strings.ReplaceAll(c.DBName, "`", "``")+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin")
		admin.Close()
		if e != nil {
			logger.Fatal("Failed to create MySQL database", zap.Error(e))
		}
		db, err = openMySQL(c)
		if err == nil {
			err = db.PingContext(pingCtx)
		}
	}
	if err != nil {
		logger.Fatal("Error connecting to MySQL", zap.Error(err))
	}
	db.SetMaxOpenConns(config.GetDatabase().MaxOpenConns)
	db.SetMaxIdleConns(config.GetDatabase().MaxIdleConns)
	db.SetConnMaxLifetime(time.Duration(config.GetDatabase().ConnMaxLifetimeMs) * time.Millisecond)
	db.SetConnMaxIdleTime(3 * time.Minute)
	var version string
	if err := db.QueryRowContext(pingCtx, "SELECT VERSION()").Scan(&version); err != nil {
		logger.Fatal("Error querying MySQL version", zap.Error(err))
	}
	logger.Info("Database information", zap.String("backend", "mysql"), zap.String("version", version))
	logger.Warn("Experimental MySQL backend: consult dev/MYSQL_RELEASE.md for verification scope and adoption requirements")
	return db
}

func openMySQL(c *mysql.Config) (*sql.DB, error) {
	connector, err := mysql.NewConnector(c)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(connector), nil
}

func mysqlErrorNumber(err error, number uint16) bool {
	var e *mysql.MySQLError
	return errors.As(err, &e) && e.Number == number
}

// The callback may execute more than once and must not perform external effects.
// Retry only server-reported deadlock/lock-timeout errors after rollback. A
// connection loss on COMMIT has an unknown outcome and must never be replayed.
func executeInTxMySQL(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return err
		}
		var rollbackErr error
		// A defer remains for panic safety; ordinary errors check rollback before retry.
		err = func() error {
			defer tx.Rollback()
			if err := fn(tx); err != nil {
				rollbackErr = tx.Rollback()
				return err
			}
			return tx.Commit()
		}()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return fmt.Errorf("MySQL rollback failed; transaction not replayed: %w", rollbackErr)
		}
		if err == nil {
			return nil
		}
		last = err
		if !mysqlErrorNumber(err, 1213) && !mysqlErrorNumber(err, 1205) {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("MySQL transaction retries exhausted: %w", last)
}
