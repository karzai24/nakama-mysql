// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"github.com/gofrs/uuid/v5"
	"strings"
)

func statusFollowUsersMySQL(ctx context.Context, db *sql.DB, ids map[uuid.UUID]struct{}, usernames map[string]struct{}) (map[uuid.UUID]string, error) {
	result := make(map[uuid.UUID]string)
	var clauses []string
	var params []any
	if len(ids) > 0 {
		clauses = append(clauses, "id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")")
		for id := range ids {
			params = append(params, id)
		}
	}
	if len(usernames) > 0 {
		clauses = append(clauses, "username IN ("+strings.TrimSuffix(strings.Repeat("?,", len(usernames)), ",")+")")
		for name := range usernames {
			params = append(params, name)
		}
	}
	if len(clauses) == 0 {
		return result, nil
	}
	rows, err := db.QueryContext(ctx, "SELECT id,username FROM users WHERE "+strings.Join(clauses, " OR "), params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err = rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		result[id] = name
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
