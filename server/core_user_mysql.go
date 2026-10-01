// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"errors"
	"strings"

	"github.com/gofrs/uuid/v5"
)

const mysqlUserColumns = `SELECT id, username, display_name, avatar_url, lang_tag, location, timezone, metadata,
    apple_id, facebook_id, facebook_instant_game_id, google_id, gamecenter_id, steam_id, edge_count, create_time, update_time FROM users`

func mysqlMarks(count int) string { return strings.TrimSuffix(strings.Repeat("?,", count), ",") }
func mysqlStringArgs(values []string) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}
func mysqlUUIDArgs(values []uuid.UUID) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value.String()
	}
	return args
}

// Columns are internal literals; all caller-supplied values remain bound parameters.
func userLookupMySQL(ids, usernames, fbIDs []string) (string, []any) {
	clauses, args := []string{}, []any{}
	for i, values := range [][]string{ids, usernames, fbIDs} {
		if len(values) == 0 {
			continue
		}
		clauses = append(clauses, []string{"id", "username", "facebook_id"}[i]+" IN ("+mysqlMarks(len(values))+")")
		args = append(args, mysqlStringArgs(values)...)
	}
	if len(clauses) == 0 {
		return mysqlUserColumns + " WHERE FALSE", args
	}
	return mysqlUserColumns + " WHERE " + strings.Join(clauses, " OR "), args
}

func accountLookupMySQL(userIDs, deviceIDs []string) (string, []any) {
	query := `SELECT u.id, u.username, u.display_name, u.avatar_url, u.lang_tag, u.location, u.timezone, u.metadata, u.wallet,
        u.email, u.apple_id, u.facebook_id, u.facebook_instant_game_id, u.google_id, u.gamecenter_id, u.steam_id, u.custom_id, u.edge_count,
        u.create_time, u.update_time, u.verify_time, u.disable_time,
        COALESCE((SELECT JSON_ARRAYAGG(ud.id) FROM user_device ud WHERE ud.user_id = u.id), JSON_ARRAY()) FROM users u WHERE `
	clauses, args := []string{}, []any{}
	if len(userIDs) > 0 {
		clauses = append(clauses, "u.id IN ("+mysqlMarks(len(userIDs))+")")
		args = append(args, mysqlStringArgs(userIDs)...)
	}
	if len(deviceIDs) > 0 {
		clauses = append(clauses, "u.id IN (SELECT user_id FROM user_device WHERE id IN ("+mysqlMarks(len(deviceIDs))+"))")
		args = append(args, mysqlStringArgs(deviceIDs)...)
	}
	if len(clauses) == 0 {
		return query + "FALSE", args
	}
	return query + strings.Join(clauses, " OR "), args
}

// PostgreSQL UUID comparisons accept uppercase input; CHAR UUIDs need explicit
// canonicalization. Do not turn malformed input into the system user's nil UUID.
func canonicalUserIDsMySQL(ids []string) ([]string, error) {
	canonical := make([]string, len(ids))
	for i, id := range ids {
		parsed, err := uuid.FromString(id)
		if err != nil {
			return nil, errors.New("invalid user ID")
		}
		canonical[i] = parsed.String()
	}
	return canonical, nil
}
