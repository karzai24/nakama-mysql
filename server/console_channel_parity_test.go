// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"testing"
	"time"

	"github.com/heroiclabs/nakama/v3/console"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDualBackendConsoleChatDeletionAtomic(t *testing.T) {
	s, ctx := consoleFixture(t)
	owner, _ := lifecycleUser(t, s.db)
	stream, _ := chatRoom(t)
	first := chatSend(t, s.db, stream, owner, `{"value":1}`)
	second := chatSend(t, s.db, stream, owner, `{"value":2}`)
	t.Cleanup(func() {
		lifecycleExec(t, s.db, "DELETE FROM message WHERE id IN (?,?)", first.MessageId, second.MessageId)
	})
	// Restrict the time cutoff to this fixture's artificially old message.
	lifecycleExec(t, s.db, "UPDATE message SET create_time = ? WHERE id = ?", time.Unix(1, 0).UTC(), first.MessageId)
	cutoff := timestamppb.New(time.Unix(2, 0).UTC())
	_, err := s.DeleteChannelMessages(ctx, &console.DeleteChannelMessagesRequest{Before: cutoff, Ids: []string{"invalid"}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, 2, lifecycleCount(t, s.db, "SELECT COUNT(*) FROM message WHERE id IN (?,?)", first.MessageId, second.MessageId))
	_, err = s.DeleteChannelMessages(ctx, &console.DeleteChannelMessagesRequest{Before: &timestamppb.Timestamp{Seconds: 253402300800}, Ids: []string{first.MessageId}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	drop := failureTrigger(t, s.db, "message", "DELETE", "OLD.id = '"+second.MessageId+"'")
	_, err = s.DeleteChannelMessages(ctx, &console.DeleteChannelMessagesRequest{Before: cutoff, Ids: []string{second.MessageId}})
	require.Error(t, err)
	require.Equal(t, 2, lifecycleCount(t, s.db, "SELECT COUNT(*) FROM message WHERE id IN (?,?)", first.MessageId, second.MessageId))
	drop()
	out, err := s.DeleteChannelMessages(ctx, &console.DeleteChannelMessagesRequest{Before: cutoff, Ids: []string{first.MessageId, second.MessageId}})
	require.NoError(t, err)
	require.EqualValues(t, 2, out.Total)
	require.Zero(t, lifecycleCount(t, s.db, "SELECT COUNT(*) FROM message WHERE id IN (?,?)", first.MessageId, second.MessageId))
}
