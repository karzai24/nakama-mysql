// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func updateAccountsMySQL(ctx context.Context, db *sql.DB, updates []*accountUpdate) error {
	return executeInTxMySQL(ctx, db, func(tx *sql.Tx) error { return updateAccountsTxMySQL(ctx, tx, updates) })
}

// Caller owns the transaction; used by standalone updates and atomic MultiUpdate.
func updateAccountsTxMySQL(ctx context.Context, tx *sql.Tx, updates []*accountUpdate) error {
	for _, update := range updates {
		sets, changed := []string{}, []string{}
		args, comparisons := []any{}, []any{}
		add := func(column string, value any, json bool) {
			placeholder := "?"
			if json {
				placeholder = "CAST(? AS JSON)"
			}
			sets = append(sets, column+" = "+placeholder)
			changed = append(changed, "NOT ("+column+" <=> "+placeholder+")")
			args, comparisons = append(args, value), append(comparisons, value)
		}
		if update.username != "" {
			if invalidUsernameRegex.MatchString(update.username) {
				return errors.New("Username invalid, no spaces or control characters allowed.")
			}
			add("username", update.username, false)
		}
		fields := []struct {
			column string
			value  *wrapperspb.StringValue
		}{
			{"display_name", update.displayName}, {"timezone", update.timezone}, {"location", update.location},
			{"lang_tag", update.langTag}, {"avatar_url", update.avatarURL},
		}
		for _, field := range fields {
			if field.value == nil {
				continue
			}
			var value any
			if field.value.Value != "" {
				value = field.value.Value
			}
			add(field.column, value, false)
		}
		if update.metadata != nil {
			add("metadata", update.metadata.Value, true)
		}
		if len(sets) == 0 {
			return errors.New("No fields to update.")
		}
		query := "UPDATE users SET update_time = CURRENT_TIMESTAMP(6), " + strings.Join(sets, ", ") + " WHERE id = ? AND (" + strings.Join(changed, " OR ") + ")"
		args = append(args, update.userID)
		args = append(args, comparisons...)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			if mysqlErrorNumber(err, 1062) {
				return status.Error(codes.AlreadyExists, "Username is already in use.")
			}
			return err
		}
	}
	return nil
}
