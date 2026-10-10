// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"testing"
	"time"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama/v3/console"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDualBackendConsoleImportValidation(t *testing.T) {
	s, ctx := consoleFixture(t)
	owner, _ := lifecycleUser(t, s.db)
	exported, err := s.ExportAccount(ctx, &console.AccountId{Id: owner.String()})
	require.NoError(t, err)
	for _, test := range []struct {
		name   string
		mutate func(*console.AccountExport)
	}{
		{"missing account", func(e *console.AccountExport) { e.Account = nil }},
		{"missing user", func(e *console.AccountExport) { e.Account.User = nil }},
		{"invalid source", func(e *console.AccountExport) { e.Account.User.Id = "bad" }},
		{"null metadata", func(e *console.AccountExport) { e.Account.User.Metadata = "null" }},
		{"array wallet", func(e *console.AccountExport) { e.Account.Wallet = "[]" }},
		{"negative wallet", func(e *console.AccountExport) { e.Account.Wallet = `{"coins":-1}` }},
		{"fractional wallet", func(e *console.AccountExport) { e.Account.Wallet = `{"coins":0.1}` }},
		{"nil object", func(e *console.AccountExport) { e.Objects = []*api.StorageObject{nil} }},
		{"bad permission", func(e *console.AccountExport) {
			now := timestamppb.New(time.Now())
			e.Objects = []*api.StorageObject{{Collection: "test", Key: "test", Value: "{}", Version: "v", PermissionRead: 3, CreateTime: now, UpdateTime: now}}
		}},
		{"missing timestamp", func(e *console.AccountExport) {
			e.Objects = []*api.StorageObject{{Collection: "test", Key: "test", Value: "{}", Version: "v"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := proto.Clone(exported).(*console.AccountExport)
			test.mutate(input)
			_, err := s.ImportAccount(ctx, &console.AccountImport{Id: owner.String(), Data: input})
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			require.Empty(t, walletBalance(t, s.db, owner))
			index := s.storageIndex.(*walletTestIndex)
			require.Empty(t, index.writes)
			require.Empty(t, index.deletes)
		})
	}
	blank := proto.Clone(exported).(*console.AccountExport)
	blank.Account.Wallet = ""
	blank.Account.User.Metadata = ""
	_, err = s.ImportAccount(ctx, &console.AccountImport{Id: owner.String(), Data: blank})
	require.NoError(t, err)
	require.Empty(t, blank.Account.Wallet, "input must remain unchanged")
	require.Empty(t, walletBalance(t, s.db, owner))
}
