// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestMySQLConfig(t *testing.T) {
	c := NewConfig(logger)
	c.GetDatabase().Addresses = []string{"mysql://user:password%40encoded@[::1]:3306/nakama?tls=true"}
	config, err := DbMySQLConfig(c)
	require.NoError(t, err)
	require.Equal(t, "password@encoded", config.Passwd)
	require.Equal(t, "[::1]:3306", config.Addr)
	require.Equal(t, "true", config.TLSConfig)
	require.True(t, config.ParseTime)
	require.False(t, config.MultiStatements)
	for _, address := range []string{
		"postgres://user:secret@host/db", "mysql://host/db", "user:secret@host/",
		"user:secret@host/db?tls=skip-verify", "user:secret@host/db?parseTime=false",
		"user:secret@host/db?tls=false&tls=true", "user:secret@host/db?multiStatements=true",
		"user:secret%broken@host/db",
	} {
		c.GetDatabase().Addresses[0] = address
		_, err := DbMySQLConfig(c)
		require.Error(t, err, address)
		require.False(t, strings.Contains(err.Error(), "secret"), "parser must not expose credentials")
	}
}

func TestDualBackendDeviceConcurrentCreate(t *testing.T) {
	db := NewDB(t)
	ctx := context.Background()
	device := "concurrent-" + GenerateString()
	const n = 16
	type result struct {
		id      string
		created bool
		err     error
	}
	results := make(chan result, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			id, _, created, err := AuthenticateDevice(ctx, logger, db, device, GenerateString(), true)
			results <- result{id, created, err}
		}()
	}
	close(start)
	owner := ""
	created := 0
	for i := 0; i < n; i++ {
		r := <-results
		require.NoError(t, r.err)
		if owner == "" {
			owner = r.id
		}
		require.Equal(t, owner, r.id)
		if r.created {
			created++
		}
	}
	require.Equal(t, 1, created)
	cleanupIdentityUser(t, db, owner)
	account, err := GetAccount(ctx, logger, db, nil, uuid.Must(uuid.FromString(owner)))
	require.NoError(t, err)
	require.Len(t, account.Devices, 1)
	require.Equal(t, device, account.Devices[0].Id)
	setIdentityBan(t, db, owner, true)
	_, _, _, err = AuthenticateDevice(ctx, logger, db, device, GenerateString(), true)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

// The same assertions execute against both engines to compare observable contracts.
func TestDualBackendStorageConcurrency(t *testing.T) {
	db := NewDB(t)
	owner := uuid.Must(uuid.NewV4())
	InsertUser(t, db, owner)
	ctx := context.Background()
	collection := "concurrency-" + GenerateString()
	makeOp := func(key, value, version string) *StorageOpWrite {
		return &StorageOpWrite{OwnerID: owner.String(), Object: &api.WriteStorageObject{
			Collection: collection, Key: key, Value: value, Version: version,
			PermissionRead: wrapperspb.Int32(1), PermissionWrite: wrapperspb.Int32(1),
		}}
	}
	initial, code, err := StorageWriteObjects(ctx, logger, db, metrics, storageIdx, false, StorageOpWrites{makeOp("shared", `{"n":0}`, "*")})
	require.NoError(t, err)
	require.Equal(t, codes.OK, code)
	version := initial.Acks[0].Version
	var successes atomic.Int32
	var unexpected atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := StorageWriteObjects(ctx, logger, db, metrics, storageIdx, false, StorageOpWrites{makeOp("shared", `{"n":1}`, version)})
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, runtime.ErrStorageRejectedVersion) {
				unexpected.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	require.EqualValues(t, 1, successes.Load())
	require.Zero(t, unexpected.Load())

	_, code, err = StorageWriteObjects(ctx, logger, db, metrics, storageIdx, false, StorageOpWrites{
		makeOp("a-must-rollback", `{"text":"فارسی 🂡"}`, "*"), makeOp("shared", `{"n":2}`, version),
	})
	require.ErrorIs(t, err, runtime.ErrStorageRejectedVersion)
	require.Equal(t, codes.InvalidArgument, code)
	objects, err := StorageReadObjects(ctx, logger, db, owner, []*api.ReadStorageObjectId{{Collection: collection, Key: "a-must-rollback", UserId: owner.String()}})
	require.NoError(t, err)
	require.Empty(t, objects.Objects)

	// Distinct case and trailing spaces must not collapse storage keys.
	_, _, err = StorageWriteObjects(ctx, logger, db, metrics, storageIdx, false, StorageOpWrites{
		makeOp("Case", `{"x":1}`, "*"), makeOp("case", `{"x":2}`, "*"), makeOp("case ", `{"x":3}`, "*"),
	})
	require.NoError(t, err)
	list, _, err := StorageListObjects(ctx, logger, db, owner, &owner, collection, 10, "")
	require.NoError(t, err)
	require.Len(t, list.Objects, 4)
	upperOwner := strings.ToUpper(owner.String())
	upper := makeOp("upper-owner", `{"ok":true}`, "*")
	upper.OwnerID = upperOwner
	_, _, err = StorageWriteObjects(ctx, logger, db, metrics, storageIdx, false, StorageOpWrites{upper})
	require.NoError(t, err)
	upperRead, err := StorageReadObjects(ctx, logger, db, owner, []*api.ReadStorageObjectId{{Collection: collection, Key: "upper-owner", UserId: upperOwner}})
	require.NoError(t, err)
	require.Len(t, upperRead.Objects, 1)
	_, err = StorageDeleteObjects(ctx, logger, db, storageIdx, false, StorageOpDeletes{{OwnerID: upperOwner, ObjectID: &api.DeleteStorageObjectId{Collection: collection, Key: "upper-owner"}}})
	require.NoError(t, err)

}

func TestDualBackendAccountUpdates(t *testing.T) {
	db := NewDB(t)
	owner := uuid.Must(uuid.NewV4())
	InsertUser(t, db, owner)
	ctx := context.Background()
	err := UpdateAccounts(ctx, logger, db, []*accountUpdate{{userID: owner, displayName: wrapperspb.String("فارسی 🂡"), metadata: wrapperspb.String(`{"nested":{"valid":true}}`)}})
	require.NoError(t, err)
	account, err := GetAccount(ctx, logger, db, nil, owner)
	require.NoError(t, err)
	require.Equal(t, "فارسی 🂡", account.User.DisplayName)
	require.JSONEq(t, `{"nested":{"valid":true}}`, account.User.Metadata)
}
