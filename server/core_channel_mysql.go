// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/heroiclabs/nakama-common/runtime"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const mysqlChannelScope = "stream_mode = ? AND stream_subject = ? AND stream_descriptor = ? AND stream_label = ?"

func channelMessagesQueryMySQL(stream PresenceStream, limit int, forward bool, cursor *channelMessageListCursor) (string, []any, error) {
	query := "SELECT id,code,sender_id,username,content,create_time,update_time FROM message WHERE " + mysqlChannelScope
	args := []any{stream.Mode, stream.Subject, stream.Subcontext, stream.Label}
	ascending := forward
	if cursor != nil {
		id, err := uuid.FromString(cursor.Id)
		if err != nil {
			return "", nil, runtime.ErrChannelCursorInvalid
		}
		ascending = forward == cursor.IsNext
		op := "<"
		if ascending {
			op = ">"
		}
		query += " AND (create_time,id) " + op + " (?,?)"
		args = append(args, time.Unix(0, cursor.CreateTime).UTC(), id)
	}
	if ascending {
		query += " ORDER BY create_time ASC,id ASC"
	} else {
		query += " ORDER BY create_time DESC,id DESC"
	}
	query += " LIMIT ?"
	args = append(args, limit+1)
	return query, args, nil
}

// Runtime callers may send authoritatively; client membership/JSON validation is
// performed by the shared realtime pipeline. Persistent updates/removes require
// both the sender and the complete channel identity to match the stored message.
func channelMessageMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, router MessageRouter, stream PresenceStream, channelID, messageID, content, senderID, username string, persist bool, code int32) (*rtapi.ChannelMessageAck, error) {
	messageID, senderID, err := canonicalChannelMessageIdentifiers(messageID, senderID)
	if err != nil {
		return nil, err
	}
	id, sender := uuid.FromStringOrNil(messageID), uuid.FromStringOrNil(senderID)
	// Match DATETIME(6) exactly, including the timestamp sent in the live ack.
	now := time.Now().UTC().Truncate(time.Microsecond)
	created, updated := now, now
	if persist {
		err = executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
			// Reset values when retrying after a confirmed rollback.
			created, updated = now, now
			var locked string
			if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", sender).Scan(&locked); err != nil {
				if errors.Is(err, sql.ErrNoRows) && code != ChannelMessageTypeChat {
					return errChannelMessageNotFound
				}
				return err
			}
			if code == ChannelMessageTypeChat {
				_, err := tx.ExecContext(ctx, "INSERT INTO message (id,code,sender_id,username,stream_mode,stream_subject,stream_descriptor,stream_label,content,create_time,update_time) VALUES (?,?,?,?,?,?,?,?,?,?,?)", id, code, sender, username, stream.Mode, stream.Subject, stream.Subcontext, stream.Label, content, now, now)
				return err
			}
			args := []any{id, sender, stream.Mode, stream.Subject, stream.Subcontext, stream.Label}
			where := "id = ? AND sender_id = ? AND " + mysqlChannelScope
			if err := tx.QueryRowContext(ctx, "SELECT create_time,update_time FROM message WHERE "+where+" FOR UPDATE", args...).Scan(&created, &updated); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return errChannelMessageNotFound
				}
				return err
			}
			if code == ChannelMessageTypeChatRemove {
				_, err := tx.ExecContext(ctx, "DELETE FROM message WHERE "+where, args...)
				return err
			}
			updated = now
			_, err := tx.ExecContext(ctx, "UPDATE message SET content = ?,username = ?,update_time = ? WHERE "+where, append([]any{content, username, updated}, args...)...)
			return err
		})
		if err != nil {
			if errors.Is(err, errChannelMessageNotFound) {
				return nil, errChannelMessageNotFound
			}
			logger.Error("Error persisting MySQL channel message", zap.Error(err))
			return nil, errChannelMessagePersist
		}
	}
	message := &api.ChannelMessage{ChannelId: channelID, MessageId: id.String(), Code: wrapperspb.Int32(code), SenderId: sender.String(), Username: username, Content: content, CreateTime: timestamppb.New(created), UpdateTime: timestamppb.New(updated), Persistent: wrapperspb.Bool(persist)}
	ack := &rtapi.ChannelMessageAck{ChannelId: message.ChannelId, MessageId: message.MessageId, Code: message.Code, Username: username, CreateTime: message.CreateTime, UpdateTime: message.UpdateTime, Persistent: message.Persistent}
	switch stream.Mode {
	case StreamModeChannel:
		message.RoomName, ack.RoomName = stream.Label, stream.Label
	case StreamModeGroup:
		message.GroupId, ack.GroupId = stream.Subject.String(), stream.Subject.String()
	case StreamModeDM:
		message.UserIdOne, ack.UserIdOne = stream.Subject.String(), stream.Subject.String()
		message.UserIdTwo, ack.UserIdTwo = stream.Subcontext.String(), stream.Subcontext.String()
	}
	// Delivery starts only after a successful commit; ambiguous commits are not
	// replayed by the transaction runner. Ephemeral messages never touch SQL.
	router.SendToStream(logger, stream, &rtapi.Envelope{Message: &rtapi.Envelope_ChannelMessage{ChannelMessage: message}}, true)
	return ack, nil
}
