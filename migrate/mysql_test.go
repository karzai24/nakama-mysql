// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/heroiclabs/nakama/v3/server"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestMySQLMigrations(t *testing.T) {
	address := os.Getenv("TEST_MYSQL_ADMIN_URL")
	if address == "" {
		t.Skip("requires local disposable-database privileges")
	}
	ctx := context.Background()
	log := zap.NewNop()
	c := server.NewConfig(log)
	c.GetDatabase().Addresses = []string{address}
	config, err := server.DbMySQLConfig(c)
	require.NoError(t, err)
	config.DBName = ""
	connector, err := mysql.NewConnector(config)
	require.NoError(t, err)
	admin := sql.OpenDB(connector)
	defer admin.Close()
	name := fmt.Sprintf("nakama_port_test_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin")
	require.NoError(t, err)
	defer admin.ExecContext(ctx, "DROP DATABASE `"+name+"`")
	config.DBName = name
	connector, err = mysql.NewConnector(config)
	require.NoError(t, err)
	db := sql.OpenDB(connector)
	defer db.Close()
	var connectedName string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&connectedName))
	require.Equal(t, name, connectedName, "all migration commands must target only this newly created disposable database")
	require.NotEqual(t, "nakama", connectedName)
	require.Error(t, CheckMySQL(ctx, db))
	// Upgrade a populated original baseline without rewriting its checksum/history.
	require.NoError(t, RunMySQL(ctx, log, db, "up", 1))
	require.Error(t, CheckMySQL(ctx, db), "startup requires every migration")
	groupID := "22222222-2222-4222-8222-222222222222"
	_, err = db.ExecContext(ctx, "INSERT INTO `groups` (id,creator_id,name,edge_count) VALUES (?,?,'upgrade-fixture',1)", groupID, groupID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO user_device (id,user_id) VALUES ('legacy-upgrade-device','00000000-0000-0000-0000-000000000000')")
	require.NoError(t, err)
	require.NoError(t, RunMySQL(ctx, log, db, "up", -1))
	require.NoError(t, CheckMySQL(ctx, db))
	require.NoError(t, RunMySQL(ctx, log, db, "up", -1))
	require.NoError(t, RunMySQL(ctx, log, db, "status", -1))

	var legacyProvider string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT provider FROM user_device WHERE id='legacy-upgrade-device'").Scan(&legacyProvider))
	require.Empty(t, legacyProvider, "3.40 device remains in the device namespace after 3.41 migration")
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM `groups` WHERE id = ?", groupID).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'groups' AND index_name = 'PRIMARY'").Scan(&count))
	require.Equal(t, 1, count, "group primary key contains only its immutable ID")
	require.NoError(t, RunMySQL(ctx, log, db, "down", 1))
	require.Error(t, CheckMySQL(ctx, db))
	require.NoError(t, RunMySQL(ctx, log, db, "down", 1))
	// A real DDL failure leaves a dirty record and prevents an automatic restart/replay.
	_, err = db.ExecContext(ctx, "CREATE UNIQUE INDEX groups_discovery_idx ON `groups` (disable_time,lang_tag,edge_count,id)")
	require.NoError(t, err)
	require.Error(t, RunMySQL(ctx, log, db, "up", 1))
	require.Error(t, CheckMySQL(ctx, db))
	require.Error(t, RunMySQL(ctx, log, db, "up", 1))
	// Explicit fixture repair after verifying MySQL rejected the atomic ALTER.
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'groups' AND index_name = 'PRIMARY'").Scan(&count))
	require.Equal(t, 4, count)
	_, err = db.ExecContext(ctx, "DROP INDEX groups_discovery_idx ON `groups`")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "DELETE FROM migration_info_mysql WHERE id = ?", mysqlVersions[1])
	require.NoError(t, err)
	require.NoError(t, RunMySQL(ctx, log, db, "up", 1))
	require.NoError(t, RunMySQL(ctx, log, db, "redo", 1))
	require.NoError(t, RunMySQL(ctx, log, db, "up", -1))
	require.NoError(t, CheckMySQL(ctx, db))
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM `groups` WHERE id = ?", groupID).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&count))
	require.Equal(t, 20, count)
	// MySQL has no SET DEFAULT foreign-key action; the trigger retains purchases.
	id := "11111111-1111-4111-8111-111111111111"
	_, err = db.ExecContext(ctx, "INSERT INTO users (id, username) VALUES (?, 'migration-test')", id)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO purchase (transaction_id, product_id, user_id) VALUES ('tx', 'product', ?)", id)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO subscription (original_transaction_id, product_id, user_id, expire_time) VALUES ('sub', 'product', ?, '2030-01-01')", id)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "DELETE FROM users WHERE id = ?", id)
	require.NoError(t, err)
	for _, table := range []string{"purchase", "subscription"} {
		var owner string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT user_id FROM "+table).Scan(&owner))
		require.Equal(t, "00000000-0000-0000-0000-000000000000", owner)
	}
	_, err = db.ExecContext(ctx, "UPDATE migration_info_mysql SET dirty = true")
	require.NoError(t, err)
	require.Error(t, CheckMySQL(ctx, db))
	require.Error(t, RunMySQL(ctx, log, db, "up", -1))
	_, err = db.ExecContext(ctx, "UPDATE migration_info_mysql SET dirty = false, checksum = 'tampered'")
	require.NoError(t, err)
	require.Error(t, CheckMySQL(ctx, db))
	require.Error(t, RunMySQL(ctx, log, db, "up", -1))
	for _, id := range mysqlVersions {
		_, err = db.ExecContext(ctx, "UPDATE migration_info_mysql SET checksum = ? WHERE id = ?", mysqlMigrationChecksum(id), id)
		require.NoError(t, err)
	}
	require.NoError(t, RunMySQL(ctx, log, db, "redo", -1))
	require.NoError(t, CheckMySQL(ctx, db))
	require.NoError(t, RunMySQL(ctx, log, db, "down", -1))
	require.Error(t, CheckMySQL(ctx, db))
	require.NoError(t, RunMySQL(ctx, log, db, "up", -1))
}
