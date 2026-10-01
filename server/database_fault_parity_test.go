// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"

	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func requireDisposableFaultTests(t *testing.T) {
	t.Helper()
	if os.Getenv("TEST_DB_FAULTS") != "true" {
		t.Skip("requires explicitly enabled disposable database fault tests")
	}
}

func TestDualBackendConnectionLossRollback(t *testing.T) {
	requireDisposableFaultTests(t)
	db := identityTestDB(t)
	owner, _ := lifecycleUser(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	calls := 0
	err := ExecuteInTx(ctx, db, func(tx *sql.Tx) error {
		calls++
		var connectionID int64
		query := "SELECT CONNECTION_ID()"
		if err := tx.QueryRowContext(ctx, query).Scan(&connectionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, lifecycleSQL(db, "UPDATE users SET wallet = ? WHERE id = ?"), `{"coins":99}`, owner); err != nil {
			return err
		}
		// The control connection terminates only this fixture's own SQL session.
		{
			_, err := db.ExecContext(ctx, fmt.Sprintf("KILL CONNECTION %d", connectionID))
			require.NoError(t, err)
		}

		return nil // The transaction wrapper now encounters the lost connection.
	})
	require.Error(t, err)
	require.Equal(t, 1, calls, "connection loss must not replay arbitrary writes")
	require.Empty(t, walletBalance(t, db, owner), "uncommitted write must roll back")
	require.NoError(t, db.PingContext(ctx), "pool must recover with a healthy connection")
}

// commitReplyDropConn consumes a real server's successful COMMIT reply and closes
// the socket before the driver sees it. The database commits; the application sees
// an error. Only the isolated fixture connection is wrapped, with TLS disabled by
// the disposable runner so protocol frames can be inspected deterministically.
type commitReplyDropConn struct {
	net.Conn
	armed, confirmed *atomic.Bool
	drop             bool
}

func (c *commitReplyDropConn) Write(p []byte) (int, error) {
	commit := false
	commit = len(p) >= 5 && p[4] == 3 && strings.EqualFold(string(p[5:]), "COMMIT")
	if commit && c.armed.CompareAndSwap(true, false) {
		c.drop = true
	}
	return c.Conn.Write(p)
}

func (c *commitReplyDropConn) Read(p []byte) (int, error) {
	if !c.drop {
		return c.Conn.Read(p)
	}
	c.drop = false
	defer c.Conn.Close()
	if err := c.Conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return 0, err
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(c.Conn, header); err != nil {
		return 0, err
	}
	size := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	if size < 1 || size > 1<<20 {
		return 0, fmt.Errorf("unexpected MySQL commit reply size %d", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(c.Conn, payload); err != nil {
		return 0, err
	}
	if payload[0] != 0 {
		return 0, fmt.Errorf("server did not confirm MySQL commit")
	}
	c.confirmed.Store(true)
	return 0, io.EOF
}

func TestDualBackendLostCommitReply(t *testing.T) {
	requireDisposableFaultTests(t)
	db := identityTestDB(t)
	owner, _ := lifecycleUser(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var armed, confirmed atomic.Bool
	armed.Store(true)
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &commitReplyDropConn{Conn: conn, armed: &armed, confirmed: &confirmed}, nil
	}
	var faulty *sql.DB
	{
		cfg := NewConfig(zap.NewNop())
		cfg.GetDatabase().Addresses = []string{os.Getenv("TEST_DB_URL")}
		native, err := DbMySQLConfig(cfg)
		require.NoError(t, err)
		require.Equal(t, "false", native.TLSConfig, "wire fault fixture must use the isolated plaintext network")
		native.DialFunc = dial
		faulty, err = openMySQL(native)
		require.NoError(t, err)
	}

	defer faulty.Close()
	result, err := UpdateWallets(ctx, zap.NewNop(), faulty, []*walletUpdate{{UserID: owner, Changeset: map[string]int64{"coins": 7}, Metadata: `{"fault":true}`}}, true)
	require.Error(t, err, "client must observe lost commit acknowledgement")
	require.Nil(t, result, "uncertain outcome must not be exposed as confirmed success")
	require.True(t, confirmed.Load(), "fault must occur after the real server confirmed COMMIT")
	require.False(t, armed.Load())
	require.Equal(t, int64(7), walletBalance(t, db, owner)["coins"], "ambiguous transaction must not be replayed")
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?", owner))
	// The next request uses a new connection and must remain usable.
	result, err = UpdateWallets(ctx, zap.NewNop(), faulty, []*walletUpdate{{UserID: owner, Changeset: map[string]int64{"coins": 1}, Metadata: `{}`}}, true)
	require.NoError(t, err)
	require.Len(t, result, 1)
	require.Equal(t, int64(8), result[0].Updated["coins"])
	require.Equal(t, 2, lifecycleCount(t, db, "SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?", owner))
}

func TestDualBackendRetryExhaustionRollsBack(t *testing.T) {
	db := identityTestDB(t)
	owner, _ := lifecycleUser(t, db)
	ctx := t.Context()
	calls := 0
	retry := error(&mysql.MySQLError{Number: 1213})

	err := ExecuteInTx(ctx, db, func(tx *sql.Tx) error {
		calls++
		if _, err := tx.ExecContext(ctx, lifecycleSQL(db, "UPDATE users SET wallet = ? WHERE id = ?"), `{"coins":99}`, owner); err != nil {
			return err
		}
		return retry
	})
	require.ErrorIs(t, err, retry)
	require.Equal(t, 5, calls)
	require.Empty(t, walletBalance(t, db, owner))

}
