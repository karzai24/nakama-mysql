// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"testing"

	"github.com/heroiclabs/nakama/v3/console"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestDualBackendConsoleFinalIdentityGuard(t *testing.T) {
	s, ctx := consoleFixture(t)
	owner, _ := lifecycleUser(t, s.db)
	lifecycleExec(t, s.db, "UPDATE users SET email = ? WHERE id = ?", "imported_"+GenerateString()+"@example.invalid", owner)
	// An imported email without a password cannot rescue the last login.
	_, err := s.UpdateAccount(ctx, &console.UpdateAccountRequest{Id: owner.String(), CustomId: wrapperspb.String(""), Wallet: wrapperspb.String(`{"coins":99}`)})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	account, err := GetAccount(ctx, s.logger, s.db, nil, owner)
	require.NoError(t, err)
	require.NotEmpty(t, account.CustomId)
	require.JSONEq(t, `{}`, account.Wallet)
}

func TestDualBackendConsoleReplaceIdentity(t *testing.T) {
	s, ctx := consoleFixture(t)
	owner, _ := lifecycleUser(t, s.db)
	device := "device_" + GenerateString()
	lifecycleExec(t, s.db, "INSERT INTO user_device (id,user_id) VALUES (?,?)", device, owner)
	lifecycleExec(t, s.db, "UPDATE users SET custom_id = NULL WHERE id = ?", owner)
	replacement := "custom_" + GenerateString()
	_, err := s.UpdateAccount(ctx, &console.UpdateAccountRequest{Id: owner.String(), CustomId: wrapperspb.String(replacement), DeviceIds: map[string]string{device: ""}})
	require.NoError(t, err, "validate login usability after all requested edits")
	account, err := GetAccount(ctx, s.logger, s.db, nil, owner)
	require.NoError(t, err)
	require.Equal(t, replacement, account.CustomId)
	require.Empty(t, account.Devices)
	lifecycleExec(t, s.db, "UPDATE users SET apple_id = ? WHERE id = ?", "apple_"+GenerateString(), owner)
	_, err = s.UpdateAccount(ctx, &console.UpdateAccountRequest{Id: owner.String(), CustomId: wrapperspb.String("")})
	require.NoError(t, err, "Apple remains a usable login")
}
