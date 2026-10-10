// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"github.com/dop251/goja"
	lua "github.com/heroiclabs/nakama/v3/internal/gopher-lua"
	"sync"
	"testing"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMySQLRegisteredProviderLifecycle(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	identity := GenerateString()
	provider := "example"
	registry := &RuntimeAuthenticateProviderRegistry{}
	require.NoError(t, registry.Register(provider, func(context.Context, string, map[string]any) (runtime.AuthenticateProviderResult, error, codes.Code) {
		return &runtime.DefaultAuthenticateProviderResult{ProviderUserID: identity, Vars: map[string]string{"role": "tester"}}, nil, codes.OK
	}, nil))
	_, _, _, _, err := Authenticate(ctx, logger, db, nil, nil, registry, "EXAMPLE", nil, "", GenerateString(), false, "")
	require.Equal(t, codes.NotFound, status.Code(err))
	id, name, created, vars, err := Authenticate(ctx, logger, db, nil, nil, registry, "EXAMPLE", nil, "", GenerateString(), true, "")
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "tester", vars["role"])
	cleanupIdentityUser(t, db, id)
	account, err := GetAccount(ctx, logger, db, nil, uuid.FromStringOrNil(id))
	require.NoError(t, err)
	require.Empty(t, account.Devices)
	require.Len(t, account.Providers, 1)
	require.Equal(t, provider, account.Providers[0].Provider)
	require.Equal(t, codes.PermissionDenied, status.Code(Unlink(ctx, logger, db, uuid.FromStringOrNil(id), provider)))
	_, _, _, err = AuthenticateDevice(ctx, logger, db, identity, GenerateString(), false)
	require.Equal(t, codes.NotFound, status.Code(err), "provider identity must not authenticate as a device")
	device := GenerateString()
	require.NoError(t, LinkDevice(ctx, logger, db, uuid.FromStringOrNil(id), device))
	account, err = GetAccount(ctx, logger, db, nil, uuid.FromStringOrNil(id))
	require.NoError(t, err)
	require.Len(t, account.Devices, 1)
	require.Len(t, account.Providers, 1)
	accounts, err := GetAccounts(ctx, logger, db, nil, nil, []string{identity})
	require.NoError(t, err)
	require.Empty(t, accounts, "device lookup must not match named provider identity")
	require.NoError(t, Link(ctx, logger, db, nil, nil, registry, uuid.FromStringOrNil(id), name, provider, nil, ""))
	other, _, _, err := AuthenticateCustom(ctx, logger, db, GenerateString(), GenerateString(), true)
	require.NoError(t, err)
	cleanupIdentityUser(t, db, other)
	require.Equal(t, codes.AlreadyExists, status.Code(Link(ctx, logger, db, nil, nil, registry, uuid.FromStringOrNil(other), name, provider, nil, "")))
	setIdentityBan(t, db, id, true)
	_, _, _, _, err = Authenticate(ctx, logger, db, nil, nil, registry, provider, nil, "", name, true, "")
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	setIdentityBan(t, db, id, false)
	require.NoError(t, Unlink(ctx, logger, db, uuid.FromStringOrNil(id), provider))
	account, err = GetAccount(ctx, logger, db, nil, uuid.FromStringOrNil(id))
	require.NoError(t, err)
	require.Empty(t, account.Providers)
	require.Len(t, account.Devices, 1)
}

func TestMySQLRegisteredProviderConcurrentCreate(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	identity := GenerateString()
	var wg sync.WaitGroup
	type result struct {
		id      string
		created bool
		err     error
	}
	results := make(chan result, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _, created, err := authenticateRegisteredProviderMySQL(ctx, db, "example", identity, "", GenerateString(), true)
			results <- result{id, created, err}
		}()
	}
	wg.Wait()
	close(results)
	winner := ""
	created := 0
	for r := range results {
		require.NoError(t, r.err)
		if winner == "" {
			winner = r.id
			cleanupIdentityUser(t, db, winner)
		}
		require.Equal(t, winner, r.id)
		if r.created {
			created++
		}
	}
	require.Equal(t, 1, created)
	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM user_device WHERE provider=? AND id=?", "example", identity).Scan(&count))
	require.Equal(t, 1, count)
}

// Exercise the actual language adapters against persisted provider identities,
// rather than treating a shared helper call as runtime-surface coverage.
func TestMySQLRegisteredProviderRuntimeAdapters(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	registry := &RuntimeAuthenticateProviderRegistry{}
	require.NoError(t, registry.Register("fixture", func(_ context.Context, _ string, payload map[string]any) (runtime.AuthenticateProviderResult, error, codes.Code) {
		return &runtime.DefaultAuthenticateProviderResult{ProviderUserID: payload["id"].(string)}, nil, codes.OK
	}, nil))
	t.Run("go", func(t *testing.T) {
		module := &RuntimeGoNakamaModule{db: db, logger: logger, authProviderRegistry: registry}
		id, _, created, err := module.Authenticate(ctx, "fixture", map[string]any{"id": GenerateString()}, "", "", true)
		require.NoError(t, err)
		require.True(t, created)
		cleanupIdentityUser(t, db, id)
		account, err := GetAccount(ctx, logger, db, nil, uuid.FromStringOrNil(id))
		require.NoError(t, err)
		require.Len(t, account.Providers, 1)
	})
	t.Run("javascript", func(t *testing.T) {
		vm := goja.New()
		module := &RuntimeJavascriptNakamaModule{ctx: ctx, db: db, logger: logger, authProviderRegistry: registry}
		require.NoError(t, vm.Set("authenticate", module.authenticate(vm)))
		require.NoError(t, vm.Set("identity", GenerateString()))
		value, err := vm.RunString(`var result=authenticate("fixture",{id:identity}); if(!result.created) throw Error("not created"); result.userId;`)
		require.NoError(t, err)
		id := value.String()
		cleanupIdentityUser(t, db, id)
		account, err := GetAccount(ctx, logger, db, nil, uuid.FromStringOrNil(id))
		require.NoError(t, err)
		require.Len(t, account.Providers, 1)
	})
	t.Run("lua", func(t *testing.T) {
		vm := lua.NewState()
		defer vm.Close()
		vm.SetContext(ctx)
		module := &RuntimeLuaNakamaModule{db: db, logger: logger, authProviderRegistry: registry}
		vm.SetGlobal("authenticate", vm.NewFunction(module.authenticate))
		vm.SetGlobal("identity", lua.LString(GenerateString()))
		require.NoError(t, vm.DoString(`user_id,username,created=authenticate("fixture",{id=identity}); assert(created)`))
		id := vm.GetGlobal("user_id").String()
		cleanupIdentityUser(t, db, id)
		account, err := GetAccount(ctx, logger, db, nil, uuid.FromStringOrNil(id))
		require.NoError(t, err)
		require.Len(t, account.Providers, 1)
	})
}
