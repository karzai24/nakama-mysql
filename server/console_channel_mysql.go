// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama/v3/console"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func deleteChannelMessages(ctx context.Context, db *sql.DB, in *console.DeleteChannelMessagesRequest) (*console.DeleteChannelMessagesResponse, error) {
	ids := make([]any, len(in.Ids))

	for i, s := range in.Ids {
		id, err := uuid.FromString(s)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "Requires a valid message ID.")
		}
		ids[i] = id

	}
	if in.Before != nil {
		if err := in.Before.CheckValid(); err != nil {
			return nil, status.Error(codes.InvalidArgument, "Requires a valid timestamp.")
		}
	}
	var affected int64
	err := ExecuteInTx(ctx, db, func(tx *sql.Tx) error {
		affected = 0
		if in.Before != nil {
			query := "DELETE FROM message WHERE create_time < ?"
			result, err := tx.ExecContext(ctx, query, in.Before.AsTime())
			if err != nil {
				return err
			}
			affected, err = result.RowsAffected()
			if err != nil {
				return err
			}
		}
		if len(ids) > 0 {
			query, args := "DELETE FROM message WHERE id IN ("+mysqlMarks(len(ids))+")", ids
			{
				query, args = "DELETE FROM message WHERE id IN ("+mysqlMarks(len(ids))+")", ids
			}
			result, err := tx.ExecContext(ctx, query, args...)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			affected += n
		}
		return nil
	})
	if err != nil {
		return nil, status.Error(codes.Internal, "An error occurred while trying to delete messages.")
	}
	return &console.DeleteChannelMessagesResponse{Total: affected}, nil
}
