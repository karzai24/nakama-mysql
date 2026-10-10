package server

import (
	"context"
	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestServer_ListFriendsOfFriends(t *testing.T) {
	ctx := context.Background()

	db := NewDB(t)

	sessionRegistry := NewLocalSessionRegistry(metrics)
	statusRegistry := NewLocalStatusRegistry(logger, cfg, sessionRegistry, protojsonMarshaler)
	t.Cleanup(statusRegistry.Stop)

	// user
	uid := uuid.Must(uuid.NewV4())

	uidA1 := uuid.Must(uuid.NewV4()) // friend of uid
	uidA2 := uuid.Must(uuid.NewV4()) // friend of A1
	uidA3 := uuid.Must(uuid.NewV4()) // friend of A1

	uidB1 := uuid.Must(uuid.NewV4()) // friend of uid
	uidB2 := uuid.Must(uuid.NewV4()) // friend of B1
	uidB3 := uuid.Must(uuid.NewV4()) // friend of uid, B1

	InsertUser(t, db, uid)

	t.Run("returns empty list if the user has no friends", func(t *testing.T) {
		fof, err := ListFriendsOfFriends(ctx, logger, db, statusRegistry, uid, 100, "")
		if err != nil {
			t.Fatal(err)
		}

		assert.Empty(t, fof.FriendsOfFriends)
	})

	InsertUser(t, db, uidA1)
	InsertUser(t, db, uidA2)
	InsertUser(t, db, uidA3)
	InsertUser(t, db, uidB1)
	InsertUser(t, db, uidB2)
	InsertUser(t, db, uidB3)

	// Use the public core so fixtures follow the selected database backend.
	for _, pair := range [][2]uuid.UUID{
		{uid, uidA1},
		{uidA1, uidA2},
		{uidA1, uidA3},
		{uid, uidB1},
		{uidB1, uidB2},
		{uidB1, uidB3},
		{uid, uidB3},
	} {
		for _, edge := range [][2]uuid.UUID{pair, {pair[1], pair[0]}} {
			if err := AddFriends(ctx, logger, db, &friendTracker{}, nil, edge[0], "fixture", []string{edge[1].String()}, ""); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("returns friends of friends, excluding friends in common", func(t *testing.T) {
		fof, err := ListFriendsOfFriends(ctx, logger, db, statusRegistry, uid, 100, "")
		if err != nil {
			t.Fatal(err)
		}

		resultMap := make(map[string]*api.FriendsOfFriendsList_FriendOfFriend, len(fof.FriendsOfFriends))
		for _, u := range fof.FriendsOfFriends {
			resultMap[u.User.Id] = u
		}

		assert.Len(t, fof.FriendsOfFriends, 3)
		assert.Equal(t, resultMap[uidA2.String()].Referrer, uidA1.String())
		assert.Equal(t, resultMap[uidA3.String()].Referrer, uidA1.String())
		assert.Equal(t, resultMap[uidB2.String()].Referrer, uidB1.String())
	})

	t.Run("returns a cursor if there's more pages to fetch", func(t *testing.T) {
		limit := 1
		fof, err := ListFriendsOfFriends(ctx, logger, db, statusRegistry, uid, limit, "")
		if err != nil {
			t.Fatal(err)
		}

		assert.Len(t, fof.FriendsOfFriends, limit)
		assert.NotEmpty(t, fof.Cursor)
	})

	t.Run("returns the following page if a cursor is provided", func(t *testing.T) {
		limit := 2
		fof, err := ListFriendsOfFriends(ctx, logger, db, statusRegistry, uid, limit, "")
		if err != nil {
			t.Fatal(err)
		}

		assert.Len(t, fof.FriendsOfFriends, limit)

		fof, err = ListFriendsOfFriends(ctx, logger, db, statusRegistry, uid, limit, fof.Cursor)
		if err != nil {
			t.Fatal(err)
		}

		assert.Len(t, fof.FriendsOfFriends, 1)
		assert.Empty(t, fof.Cursor)
	})
}
