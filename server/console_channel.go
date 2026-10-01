package server

import (
	"context"
	"net/url"
	"sort"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/heroiclabs/nakama/v3/console"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *ConsoleServer) ListChannelMessages(ctx context.Context, in *console.ListChannelMessagesRequest) (*api.ChannelMessageList, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	const limit = 50

	stream, err := buildStream(in)
	if err != nil {
		return nil, err
	}

	channelId, err := StreamToChannelId(*stream)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cursor, err := url.QueryUnescape(in.Cursor)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Cursor is invalid or expired.")
	}

	messageList, err := ChannelMessagesList(ctx, logger, s.db, uuid.Nil, *stream, channelId, limit, false, cursor)
	if err == runtime.ErrChannelCursorInvalid {
		return nil, status.Error(codes.InvalidArgument, "Cursor is invalid or expired.")
	} else if err != nil {
		return nil, status.Error(codes.Internal, "Error listing messages from channel.")
	}

	return messageList, nil
}

func (s *ConsoleServer) DeleteChannelMessages(ctx context.Context, in *console.DeleteChannelMessagesRequest) (*console.DeleteChannelMessagesResponse, error) {
	return deleteChannelMessages(ctx, s.db, in)
}

func buildStream(in *console.ListChannelMessagesRequest) (*PresenceStream, error) {
	stream := PresenceStream{}
	var err error
	switch in.Type {
	case console.ListChannelMessagesRequest_ROOM:
		stream.Mode = StreamModeChannel
		if l := len(in.Label); l < 1 || l > 64 {
			return nil, status.Error(codes.InvalidArgument, "Invalid label size.")
		}
		stream.Label = in.Label
	case console.ListChannelMessagesRequest_GROUP:
		stream.Mode = StreamModeGroup
		if stream.Subject, err = uuid.FromString(in.GroupId); err != nil {
			return nil, status.Error(codes.InvalidArgument, "Invalid group ID format.")
		}
	case console.ListChannelMessagesRequest_DIRECT:
		stream.Mode = StreamModeDM
		users := []string{in.UserIdOne, in.UserIdTwo}
		sort.Strings(users)
		if stream.Subject, err = uuid.FromString(users[0]); err != nil {
			return nil, status.Error(codes.InvalidArgument, "Invalid user ID format.")
		}
		if stream.Subcontext, err = uuid.FromString(users[1]); err != nil {
			return nil, status.Error(codes.InvalidArgument, "Invalid user ID format.")
		}
	default:
		return nil, status.Error(codes.Internal, "Invalid chat type.")
	}
	return &stream, nil
}
