// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"testing"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama/v3/console"
	"github.com/heroiclabs/nakama/v3/console/acl"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDualBackendConsoleRootProtection(t *testing.T) {
	s, ctx := consoleFixture(t)
	lifecycleExec(t, s.db, "DELETE FROM console_user WHERE id = ?", uuid.Nil)
	t.Cleanup(func() { lifecycleExec(t, s.db, "DELETE FROM console_user WHERE id = ?", uuid.Nil) })
	name := s.config.GetConsole().Username
	password := s.config.GetConsole().Password
	first, err := s.Authenticate(ctx, &console.AuthenticateRequest{Username: name, Password: password})
	require.NoError(t, err)
	_, err = s.Authenticate(ctx, &console.AuthenticateRequest{Username: name, Password: password})
	require.NoError(t, err)
	// A new cache represents another process/restart. Ordinary login must not
	// revoke an existing root session merely because bcrypt chose a new salt.
	fresh := NewLocalSessionCache(s.config.GetConsole().TokenExpirySec, 0)
	defer fresh.Stop()
	_, err = checkAuth(ctx, s.logger, s.db, s.config, "Bearer "+first.Token, "/nakama.console.Console/ListUsers", fresh, s.loginAttemptCache)
	require.NoError(t, err)
	_, err = s.dbInsertConsoleUser(ctx, s.logger, &console.AddUserRequest{Username: name, Email: "root-replace@example.invalid", Acl: acl.Admin().ACL()})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = s.UpdateUser(ctx, &console.UpdateUserRequest{Username: name, Acl: map[string]*console.Permissions{"ACCOUNT": {Read: true}}})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = s.ResetUserPassword(ctx, &console.Username{Username: name})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = s.DeleteUser(ctx, &console.Username{Username: name})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	// A configuration change advances the persisted revision and invalidates
	// sessions independently of a process-local revocation list.
	s.config.GetConsole().Password = "changed-root-password-123"
	second, err := s.Authenticate(ctx, &console.AuthenticateRequest{Username: name, Password: s.config.GetConsole().Password})
	require.NoError(t, err)
	_, err = checkAuth(ctx, s.logger, s.db, s.config, "Bearer "+first.Token, "/nakama.console.Console/ListUsers", fresh, s.loginAttemptCache)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	_, err = checkAuth(ctx, s.logger, s.db, s.config, "Bearer "+second.Token, "/nakama.console.Console/ListUsers", fresh, s.loginAttemptCache)
	require.NoError(t, err)
}
