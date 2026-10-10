// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/gob"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/heroiclabs/nakama/v3/console"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func chatRoom(t *testing.T) (PresenceStream, string) {
	t.Helper()
	stream := PresenceStream{Mode: StreamModeChannel, Label: "chat_" + GenerateString()}
	channel, err := StreamToChannelId(stream)
	require.NoError(t, err)
	return stream, channel
}
func chatSend(t *testing.T, db *sql.DB, stream PresenceStream, uid uuid.UUID, content string) *rtapi.ChannelMessageAck {
	t.Helper()
	channel, err := StreamToChannelId(stream)
	require.NoError(t, err)
	ack, err := ChannelMessageSend(context.Background(), logger, db, &groupRouter{}, stream, channel, content, uid.String(), "fixture", true)
	require.NoError(t, err)
	return ack
}
func chatList(t *testing.T, db *sql.DB, caller uuid.UUID, stream PresenceStream, limit int, forward bool, cursor string) *api.ChannelMessageList {
	t.Helper()
	channel, err := StreamToChannelId(stream)
	require.NoError(t, err)
	page, err := ChannelMessagesList(context.Background(), logger, db, caller, stream, channel, limit, forward, cursor)
	require.NoError(t, err)
	return page
}
func chatIDs(messages []*api.ChannelMessage) []string {
	ids := make([]string, len(messages))
	for i, m := range messages {
		ids[i] = m.MessageId
	}
	return ids
}

func TestDualBackendChatLifecycle(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	sender, _ := lifecycleUser(t, db)
	other, _ := lifecycleUser(t, db)
	group := groupFixture(t, db, sender, true, 4)
	for _, stream := range []PresenceStream{{Mode: StreamModeChannel, Label: "room_" + GenerateString()}, {Mode: StreamModeGroup, Subject: group}, {Mode: StreamModeDM, Subject: sender, Subcontext: other}} {
		channel, err := StreamToChannelId(stream)
		require.NoError(t, err)
		router := &groupRouter{}
		router.check = func(m *api.ChannelMessage) {
			n := lifecycleCount(t, db, "SELECT COUNT(*) FROM message WHERE id = ?", m.MessageId)
			if m.Code.Value == ChannelMessageTypeChatRemove {
				require.Zero(t, n)
			} else {
				require.Equal(t, 1, n)
			}
		}
		content := `{"body":"فارسی 🂡","large":9007199254740993,"nested":{"nil":null},"items":[true,1]}`
		ack, err := ChannelMessageSend(ctx, logger, db, router, stream, channel, content, sender.String(), "sender", true)
		require.NoError(t, err)
		require.True(t, ack.Persistent.Value)
		require.Equal(t, channel, ack.ChannelId)
		require.Len(t, router.messages, 1)
		require.Equal(t, sender.String(), router.messages[0].SenderId)
		initial := chatList(t, db, sender, stream, 10, true, "")
		require.Len(t, initial.Messages, 1)
		saved := initial.Messages[0]
		require.JSONEq(t, content, saved.Content)
		require.Contains(t, saved.Content, "9007199254740993")
		require.WithinDuration(t, ack.CreateTime.AsTime(), saved.CreateTime.AsTime(), time.Microsecond)
		switch stream.Mode {
		case StreamModeChannel:
			require.Equal(t, stream.Label, saved.RoomName)
		case StreamModeGroup:
			require.Equal(t, group.String(), saved.GroupId)
		case StreamModeDM:
			require.Equal(t, sender.String(), saved.UserIdOne)
			require.Equal(t, other.String(), saved.UserIdTwo)
		}
		_, err = ChannelMessageUpdate(ctx, logger, db, router, stream, channel, ack.MessageId, `{"bad":true}`, other.String(), "other", true)
		require.ErrorIs(t, err, errChannelMessageNotFound)
		_, err = ChannelMessageRemove(ctx, logger, db, router, stream, channel, ack.MessageId, other.String(), "other", true)
		require.ErrorIs(t, err, errChannelMessageNotFound)
		require.Len(t, router.messages, 1)
		changed, err := ChannelMessageUpdate(ctx, logger, db, router, stream, channel, ack.MessageId, `{"edited":"new"}`, sender.String(), "renamed", true)
		require.NoError(t, err)
		require.True(t, changed.CreateTime.AsTime().Equal(saved.CreateTime.AsTime()))
		require.EqualValues(t, ChannelMessageTypeChatUpdate, changed.Code.Value)
		page := chatList(t, db, sender, stream, 10, true, "")
		require.Len(t, page.Messages, 1)
		require.Equal(t, ack.MessageId, page.Messages[0].MessageId)
		require.Equal(t, "renamed", page.Messages[0].Username)
		require.JSONEq(t, `{"edited":"new"}`, page.Messages[0].Content)
		// History retains the original message code; edits/removals are live events.
		require.EqualValues(t, ChannelMessageTypeChat, page.Messages[0].Code.Value)
		exported, err := GetChannelMessages(ctx, logger, db, sender)
		require.NoError(t, err)
		require.Len(t, exported, 1)
		require.Equal(t, channel, exported[0].ChannelId)
		removed, err := ChannelMessageRemove(ctx, logger, db, router, stream, channel, ack.MessageId, sender.String(), "renamed", true)
		require.NoError(t, err)
		require.True(t, removed.CreateTime.AsTime().Equal(saved.CreateTime.AsTime()))
		require.True(t, removed.UpdateTime.AsTime().Equal(page.Messages[0].UpdateTime.AsTime()))
		require.Empty(t, chatList(t, db, sender, stream, 10, true, "").Messages)
		require.Len(t, router.messages, 3)
		_, err = ChannelMessageRemove(ctx, logger, db, router, stream, channel, ack.MessageId, sender.String(), "renamed", true)
		require.ErrorIs(t, err, errChannelMessageNotFound)
	}
}

func TestDualBackendChatPagination(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	sender, _ := lifecycleUser(t, db)
	stream, channel := chatRoom(t)
	stamp := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	for i := 0; i < 7; i++ {
		ack := chatSend(t, db, stream, sender, `{"item":true}`)
		lifecycleExec(t, db, "UPDATE message SET create_time = ?,update_time = ? WHERE id = ?", stamp, stamp, ack.MessageId)
	}
	for _, forward := range []bool{true, false} {
		all := chatList(t, db, sender, stream, 100, forward, "")
		require.Len(t, all.Messages, 7)
		cursor := ""
		got := []string{}
		pages := []*api.ChannelMessageList{}
		for i := 0; i < 6; i++ {
			page := chatList(t, db, sender, stream, 2, forward, cursor)
			pages = append(pages, page)
			got = append(got, chatIDs(page.Messages)...)
			cursor = page.NextCursor
			if cursor == "" {
				break
			}
		}
		require.Empty(t, cursor)
		require.Equal(t, chatIDs(all.Messages), got)
		require.Len(t, pages, 4)
		for i := len(pages) - 1; i > 0; i-- {
			previous := chatList(t, db, sender, stream, 2, forward, pages[i].PrevCursor)
			require.Equal(t, chatIDs(pages[i-1].Messages), chatIDs(previous.Messages))
		}
		// A cursor from another direction/channel is rejected before executing SQL.
		_, err := ChannelMessagesList(ctx, logger, db, sender, stream, channel, 2, !forward, pages[0].NextCursor)
		require.ErrorIs(t, err, runtime.ErrChannelCursorInvalid)
		wrong := stream
		wrong.Label += "other"
		_, err = ChannelMessagesList(ctx, logger, db, sender, wrong, channel, 2, forward, pages[0].NextCursor)
		require.ErrorIs(t, err, runtime.ErrChannelCursorInvalid)
	}
	page := chatList(t, db, sender, stream, 100, true, "")
	empty := chatList(t, db, sender, stream, 100, true, page.CacheableCursor)
	require.Empty(t, empty.Messages)
	require.Equal(t, page.CacheableCursor, empty.CacheableCursor)
	newest := chatSend(t, db, stream, sender, `{"new":true}`)
	refresh := chatList(t, db, sender, stream, 100, true, empty.CacheableCursor)
	require.Equal(t, []string{newest.MessageId}, chatIDs(refresh.Messages))
	// Removing the anchor row does not make the cursor unusable.
	first := chatList(t, db, sender, stream, 2, true, "")
	want := chatList(t, db, sender, stream, 100, true, first.NextCursor)
	_, err := ChannelMessageRemove(ctx, logger, db, &groupRouter{}, stream, channel, first.Messages[1].MessageId, sender.String(), "fixture", true)
	require.NoError(t, err)
	require.Equal(t, chatIDs(want.Messages), chatIDs(chatList(t, db, sender, stream, 100, true, first.NextCursor).Messages))
	for _, cursor := range []string{"bad!", base64.URLEncoding.EncodeToString([]byte("not gob"))} {
		_, err := ChannelMessagesList(ctx, logger, db, sender, stream, channel, 2, true, cursor)
		require.ErrorIs(t, err, runtime.ErrChannelCursorInvalid)
	}
	// Decode compatibility with the original standard-base64 cursor encoding.
	raw, err := base64.URLEncoding.DecodeString(first.NextCursor)
	require.NoError(t, err)
	legacy := chatList(t, db, sender, stream, 100, true, base64.StdEncoding.EncodeToString(raw))
	require.Equal(t, chatIDs(want.Messages), chatIDs(legacy.Messages))
}

func TestDualBackendChatAccessAndIsolation(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	a, _ := lifecycleUser(t, db)
	b, _ := lifecycleUser(t, db)
	outsider, _ := lifecycleUser(t, db)
	gid := groupFixture(t, db, a, false, 5)
	groupStream := PresenceStream{Mode: StreamModeGroup, Subject: gid}
	channel, err := StreamToChannelId(groupStream)
	require.NoError(t, err)
	chatSend(t, db, groupStream, a, `{"group":true}`)
	require.NoError(t, JoinGroup(ctx, logger, db, &groupTracker{}, &groupRouter{}, gid, b, "b"))
	_, err = ChannelMessagesList(ctx, logger, db, b, groupStream, channel, 10, true, "")
	require.ErrorIs(t, err, runtime.ErrChannelGroupNotFound)
	groupAdd(t, db, gid, a, b)
	require.Len(t, chatList(t, db, b, groupStream, 100, true, "").Messages, 2)
	require.NoError(t, BanGroupUsers(ctx, logger, db, &groupTracker{}, &groupRouter{}, &groupManager{}, a, gid, []uuid.UUID{b}))
	_, err = ChannelMessagesList(ctx, logger, db, b, groupStream, channel, 10, true, "")
	require.ErrorIs(t, err, runtime.ErrChannelGroupNotFound)
	require.Len(t, chatList(t, db, uuid.Nil, groupStream, 100, true, "").Messages, 3)
	dm, stream, err := BuildChannelId(ctx, logger, db, a, b.String(), rtapi.ChannelJoin_DIRECT_MESSAGE)
	require.NoError(t, err)
	reverse, _, err := BuildChannelId(ctx, logger, db, b, a.String(), rtapi.ChannelJoin_DIRECT_MESSAGE)
	require.NoError(t, err)
	require.Equal(t, dm, reverse)
	chatSend(t, db, stream, a, `{"private":true}`)
	require.Len(t, chatList(t, db, b, stream, 100, true, "").Messages, 1)
	_, err = ChannelMessagesList(ctx, logger, db, outsider, stream, dm, 10, true, "")
	require.ErrorIs(t, err, runtime.ErrChannelIDInvalid)
	require.NoError(t, BlockFriends(ctx, logger, db, &friendTracker{}, b, []string{a.String()}))
	_, _, err = BuildChannelId(ctx, logger, db, a, b.String(), rtapi.ChannelJoin_DIRECT_MESSAGE)
	require.ErrorIs(t, err, runtime.ErrInvalidChannelTarget)
	// Upstream permits participants to read old DM history after a block.
	require.Len(t, chatList(t, db, a, stream, 100, true, "").Messages, 1)
	room, _ := chatRoom(t)
	chatSend(t, db, room, a, `{"room":true}`)
	require.Len(t, chatList(t, db, outsider, room, 100, true, "").Messages, 1)
	sibling := room
	sibling.Label += "suffix"
	require.Empty(t, chatList(t, db, a, sibling, 100, true, "").Messages)
}

func TestDualBackendChatFailureAndEphemeral(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	sender, _ := lifecycleUser(t, db)
	stream, channel := chatRoom(t)
	r := &groupRouter{}
	for _, bad := range []struct{ content, username string }{{"{invalid", "fixture"}, {`{"ok":true}`, strings.Repeat("x", 129)}} {
		ack, err := ChannelMessageSend(ctx, zap.NewNop(), db, r, stream, channel, bad.content, sender.String(), bad.username, true)
		require.ErrorIs(t, err, errChannelMessagePersist)
		require.Nil(t, ack)
	}
	require.Empty(t, r.messages)
	require.Empty(t, chatList(t, db, sender, stream, 100, true, "").Messages)
	persisted := chatSend(t, db, stream, sender, `{"original":true}`)
	_, err := ChannelMessageUpdate(ctx, zap.NewNop(), db, r, stream, channel, persisted.MessageId, "{bad", sender.String(), "fixture", true)
	require.ErrorIs(t, err, errChannelMessagePersist)
	require.JSONEq(t, `{"original":true}`, chatList(t, db, sender, stream, 100, true, "").Messages[0].Content)
	require.Empty(t, r.messages)
	// Closing a separate pool proves all ephemeral operations avoid the database.
	closed := identityTestDB(t)
	require.NoError(t, closed.Close())
	ack, err := ChannelMessageSend(ctx, logger, closed, r, stream, channel, `{"live":true}`, sender.String(), "fixture", false)
	require.NoError(t, err)
	require.False(t, ack.Persistent.Value)
	_, err = ChannelMessageUpdate(ctx, logger, closed, r, stream, channel, ack.MessageId, `{"edited":true}`, sender.String(), "fixture", false)
	require.NoError(t, err)
	_, err = ChannelMessageRemove(ctx, logger, closed, r, stream, channel, ack.MessageId, sender.String(), "fixture", false)
	require.NoError(t, err)
	require.Len(t, r.messages, 3)
	require.Equal(t, []string{persisted.MessageId}, chatIDs(chatList(t, db, sender, stream, 100, true, "").Messages))
	// These failures cannot emit a live event or acknowledge a durable write.
	_, err = ChannelMessageSend(ctx, zap.NewNop(), closed, r, stream, channel, `{}`, sender.String(), "fixture", true)
	require.ErrorIs(t, err, errChannelMessagePersist)
	_, err = ChannelMessageRemove(ctx, zap.NewNop(), closed, r, stream, channel, persisted.MessageId, sender.String(), "fixture", true)
	require.ErrorIs(t, err, errChannelMessagePersist)
	require.Len(t, r.messages, 3)
}

func TestDualBackendChatScopeAndIdentifiers(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	uid, _ := lifecycleUser(t, db)
	stream, channel := chatRoom(t)
	ack := chatSend(t, db, stream, uid, `{"original":true}`)
	r := &groupRouter{}
	for _, other := range []PresenceStream{{Mode: StreamModeChannel, Label: stream.Label + "other"}, {Mode: StreamModeGroup, Subject: uuid.Must(uuid.NewV4()), Label: stream.Label}, {Mode: StreamModeChannel, Subject: uuid.Must(uuid.NewV4()), Label: stream.Label}, {Mode: StreamModeChannel, Subcontext: uuid.Must(uuid.NewV4()), Label: stream.Label}} {
		_, err := ChannelMessageUpdate(ctx, logger, db, r, other, channel, ack.MessageId, `{"wrong":true}`, uid.String(), "fixture", true)
		require.ErrorIs(t, err, errChannelMessageNotFound)
		_, err = ChannelMessageRemove(ctx, logger, db, r, other, channel, ack.MessageId, uid.String(), "fixture", true)
		require.ErrorIs(t, err, errChannelMessageNotFound)
	}
	require.Empty(t, r.messages)
	updated, err := ChannelMessageUpdate(ctx, logger, db, r, stream, channel, strings.ToUpper(ack.MessageId), `{"updated":true}`, strings.ToUpper(uid.String()), "fixture", true)
	require.NoError(t, err)
	require.Equal(t, ack.MessageId, updated.MessageId)
	require.Equal(t, uid.String(), r.messages[0].SenderId)
	for _, persistent := range []bool{true, false} {
		_, err = ChannelMessageUpdate(ctx, logger, db, r, stream, channel, "invalid", `{}`, uid.String(), "fixture", persistent)
		require.ErrorIs(t, err, errChannelMessageNotFound)
		_, err = ChannelMessageRemove(ctx, logger, db, r, stream, channel, "invalid", uid.String(), "fixture", persistent)
		require.ErrorIs(t, err, errChannelMessageNotFound)
	}
	_, err = ChannelMessageSend(ctx, logger, db, r, stream, channel, `{}`, "invalid", "fixture", true)
	require.ErrorIs(t, err, errChannelMessagePersist)
	// The Go runtime's documented empty-sender default resolves to the system user.
	module := &RuntimeGoNakamaModule{db: db, logger: logger, router: r}
	system, err := module.ChannelMessageSend(ctx, channel, map[string]interface{}{"system": true}, "", "", true)
	require.NoError(t, err)
	t.Cleanup(func() { lifecycleExec(t, db, "DELETE FROM message WHERE id = ?", system.MessageId) })
	require.Equal(t, uuid.Nil.String(), r.messages[len(r.messages)-1].SenderId)
	page := chatList(t, db, uid, stream, 100, true, "")
	require.Len(t, page.Messages, 2)
	for _, m := range page.Messages {
		if m.MessageId == system.MessageId {
			require.True(t, m.CreateTime.AsTime().Equal(system.CreateTime.AsTime()))
		}
	}
	var buf bytes.Buffer
	require.NoError(t, gob.NewEncoder(&buf).Encode(&channelMessageListCursor{StreamMode: stream.Mode, StreamSubject: stream.Subject.String(), StreamSubcontext: stream.Subcontext.String(), StreamLabel: stream.Label, Id: "invalid", Forward: true, IsNext: true}))
	_, err = ChannelMessagesList(ctx, logger, db, uid, stream, channel, 10, true, base64.URLEncoding.EncodeToString(buf.Bytes()))
	require.ErrorIs(t, err, runtime.ErrChannelCursorInvalid)
}

func TestDualBackendChatConsoleHandlers(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	uid, _ := lifecycleUser(t, db)
	stream, _ := chatRoom(t)
	first := chatSend(t, db, stream, uid, `{"one":1}`)
	second := chatSend(t, db, stream, uid, `{"two":2}`)
	server := &ConsoleServer{db: db, logger: logger}
	page, err := server.ListChannelMessages(ctx, &console.ListChannelMessagesRequest{Type: console.ListChannelMessagesRequest_ROOM, Label: stream.Label})
	require.NoError(t, err)
	require.Len(t, page.Messages, 2)
	result, err := server.DeleteChannelMessages(ctx, &console.DeleteChannelMessagesRequest{Ids: []string{strings.ToUpper(first.MessageId), first.MessageId, uuid.Must(uuid.NewV4()).String()}})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Total)
	require.Equal(t, []string{second.MessageId}, chatIDs(chatList(t, db, uid, stream, 100, true, "").Messages))
	// A pre-epoch cutoff only targets this unique fixture, never ordinary dev data.
	cutoff := time.Unix(-100, 0).UTC()
	lifecycleExec(t, db, "UPDATE message SET create_time = ? WHERE id = ?", cutoff.Add(-time.Second), second.MessageId)
	{
		_, err = server.DeleteChannelMessages(ctx, &console.DeleteChannelMessagesRequest{Before: timestamppb.New(cutoff), Ids: []string{"invalid"}})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM message WHERE id = ?", second.MessageId))
	}
	result, err = server.DeleteChannelMessages(ctx, &console.DeleteChannelMessagesRequest{Before: timestamppb.New(cutoff), Ids: []string{second.MessageId}})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Total)
	require.Empty(t, chatList(t, db, uid, stream, 100, true, "").Messages)
}

func TestDualBackendChatConcurrentMutationAndDeletion(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	uid, _ := lifecycleUser(t, db)
	stream, channel := chatRoom(t)
	r := &groupRouter{}
	for i := 0; i < 6; i++ {
		ack := chatSend(t, db, stream, uid, `{"original":true}`)
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			_, err := ChannelMessageUpdate(ctx, logger, db, r, stream, channel, ack.MessageId, `{"edited":true}`, uid.String(), "fixture", true)
			results <- err
		}()
		go func() {
			<-start
			_, err := ChannelMessageRemove(ctx, logger, db, r, stream, channel, ack.MessageId, uid.String(), "fixture", true)
			results <- err
		}()
		close(start)
		for j := 0; j < 2; j++ {
			err := <-results
			require.True(t, err == nil || errors.Is(err, errChannelMessageNotFound), "%v", err)
		}
		require.Empty(t, chatList(t, db, uid, stream, 100, true, "").Messages)
	}
	// Competing removals acknowledge exactly one committed delete.
	ack := chatSend(t, db, stream, uid, `{}`)
	start := make(chan struct{})
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := ChannelMessageRemove(ctx, logger, db, r, stream, channel, ack.MessageId, uid.String(), "fixture", true)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, errChannelMessageNotFound)
		}
	}
	require.Equal(t, 1, successes)
	cache := NewLocalSessionCache(60, 60)
	defer cache.Stop()
	for i := 0; i < 6; i++ {
		sender, _ := lifecycleUser(t, db)
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			_, err := ChannelMessageSend(ctx, zap.NewNop(), db, r, stream, channel, `{}`, sender.String(), "fixture", true)
			results <- err
		}()
		go func() {
			<-start
			results <- DeleteAccount(ctx, logger, db, nil, nil, &lifecycleRanks{}, &lifecycleSessions{}, cache, lifecycleTracker{}, sender, false, nil)
		}()
		close(start)
		for j := 0; j < 2; j++ {
			err := <-results
			require.True(t, err == nil || errors.Is(err, errChannelMessagePersist), "%v", err)
		}
		require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM message WHERE sender_id = ?", sender))
	}
}
