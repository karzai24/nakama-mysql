// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"testing"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestDualBackendNotifications(t *testing.T) {
	db := NewDB(t)
	ctx := context.Background()
	owner, other := uuid.Must(uuid.NewV4()), uuid.Must(uuid.NewV4())
	InsertUser(t, db, owner)
	InsertUser(t, db, other)
	notifications := make([]*api.Notification, 0, 3)
	for i := 0; i < 3; i++ {
		notifications = append(notifications, &api.Notification{Id: GenerateString(), Subject: "test", Content: `{"n":1}`, Code: 1, SenderId: uuid.Nil.String()})
	}
	require.NoError(t, NotificationSave(ctx, logger, db, map[uuid.UUID][]*api.Notification{owner: notifications}))
	cursor := ""
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		page, err := NotificationList(ctx, logger, db, owner, 1, cursor, true)
		require.NoError(t, err)
		require.Len(t, page.Notifications, 1)
		id := page.Notifications[0].Id
		require.False(t, seen[id])
		seen[id] = true
		cursor = page.CacheableCursor
	}
	empty, err := NotificationList(ctx, logger, db, owner, 1, cursor, true)
	require.NoError(t, err)
	require.Empty(t, empty.Notifications)
	require.Equal(t, cursor, empty.CacheableCursor)
	id := notifications[0].Id
	require.NoError(t, NotificationDelete(ctx, logger, db, other, []string{id}))
	subject := "updated"
	require.NoError(t, NotificationsUpdate(ctx, logger, db, notificationUpdate{Id: uuid.Must(uuid.FromString(id)), Subject: &subject, Content: map[string]any{"n": float64(2)}}))
	found, err := NotificationsGetId(ctx, logger, db, owner.String(), id)
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Equal(t, subject, found[0].Subject)
	require.Equal(t, float64(2), found[0].Content["n"])
	wrongOwner, err := NotificationsGetId(ctx, logger, db, other.String(), id)
	require.NoError(t, err)
	require.Empty(t, wrongOwner)
	require.NoError(t, NotificationsDeleteId(ctx, logger, db, owner.String(), id))
	found, err = NotificationsGetId(ctx, logger, db, "", id)
	require.NoError(t, err)
	require.Empty(t, found)
}

func TestDualBackendStorageIndexReload(t *testing.T) {
	db := NewDB(t)
	ctx := context.Background()
	owner := uuid.Must(uuid.NewV4())
	InsertUser(t, db, owner)
	collection := GenerateString()
	_, _, err := StorageWriteObjects(ctx, logger, db, metrics, storageIdx, true, StorageOpWrites{
		{OwnerID: owner.String(), Object: &api.WriteStorageObject{Collection: collection, Key: "a", Value: `{"n":1}`, PermissionRead: wrapperspb.Int32(2)}},
		{OwnerID: owner.String(), Object: &api.WriteStorageObject{Collection: collection, Key: "b", Value: `{"n":2}`, PermissionRead: wrapperspb.Int32(0)}},
	})
	require.NoError(t, err)
	index, err := NewLocalStorageIndex(logger, db, &StorageConfig{}, metrics)
	require.NoError(t, err)
	require.NoError(t, index.CreateIndex(ctx, "reload", collection, "", []string{"n"}, nil, 10, false))
	require.NoError(t, index.CreateIndex(ctx, "reload-key", collection, "a", []string{"n"}, nil, 10, false))
	require.NoError(t, index.Load(ctx))
	objects, _, err := index.List(ctx, uuid.Nil, "reload", "", 10, nil, "")
	require.NoError(t, err)
	require.Len(t, objects.Objects, 2)
	visible, _, err := index.List(ctx, owner, "reload", "", 10, nil, "")
	require.NoError(t, err)
	require.Len(t, visible.Objects, 1)
	keyed, _, err := index.List(ctx, uuid.Nil, "reload-key", "", 10, nil, "")
	require.NoError(t, err)
	require.Len(t, keyed.Objects, 1)
	require.Equal(t, "a", keyed.Objects[0].Key)
}
