// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/gob"
	"slices"
	"strings"
	"time"

	"github.com/heroiclabs/nakama/v3/console"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type auditLogListCursorMySQL struct {
	ResourceFilterSet bool
	Timestamp         time.Time
	Username          string
	Resource          console.AclResources
	Action            console.AuditLogAction
	ID                string
	IsNext            bool

	// Filter state to ensure consistent pagination
	UsernameFilter string
	ResourceFilter *console.AclResources
	ActionFilter   console.AuditLogAction
	BeforeFilter   time.Time
	AfterFilter    time.Time
}

func auditLogEntryListMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, username string, action console.AuditLogAction, resource *console.AclResources, after, before time.Time, limit int, cursor string) (*console.AuditLogList, error) {
	var incomingCursor *auditLogListCursorMySQL
	if cursor != "" {
		// Decode the cursor
		cb, err := base64.URLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, ErrAuditLogInvalidCursor
		}
		incomingCursor = &auditLogListCursorMySQL{}
		if err := gob.NewDecoder(bytes.NewReader(cb)).Decode(incomingCursor); err != nil {
			return nil, ErrAuditLogInvalidCursor
		}

		// gob flattens a pointer to enum zero. Preserve the presence of ACCOUNT.
		if incomingCursor.ResourceFilterSet && incomingCursor.ResourceFilter == nil {
			value := console.AclResources_ACCOUNT
			incomingCursor.ResourceFilter = &value
		}
		// Validate cursor is compatible with the current request filters
		if username != "" && username != incomingCursor.UsernameFilter {
			return nil, ErrAuditLogInvalidCursor
		}
		if action != 0 && action != incomingCursor.ActionFilter {
			return nil, ErrAuditLogInvalidCursor
		}
		if resource != nil && (incomingCursor.ResourceFilter == nil || *resource != *incomingCursor.ResourceFilter) {
			return nil, ErrAuditLogInvalidCursor
		}
		if !before.IsZero() && !before.Equal(incomingCursor.BeforeFilter) {
			return nil, ErrAuditLogInvalidCursor
		}
		if !after.IsZero() && !after.Equal(incomingCursor.AfterFilter) {
			return nil, ErrAuditLogInvalidCursor
		}
	}

	if limit < 1 || limit > 100 {
		return nil, status.Error(codes.InvalidArgument, "Limit must be between 1 and 100.")
	}
	if incomingCursor != nil {
		username = incomingCursor.UsernameFilter
		action = incomingCursor.ActionFilter
		resource = incomingCursor.ResourceFilter
		after = incomingCursor.AfterFilter
		before = incomingCursor.BeforeFilter
	}
	query := `
SELECT
	id,
	console_user_id,
	console_username,
	email,
	resource,
	action,
	metadata,
	message,
	create_time
FROM
	console_audit_log`

	var order string
	params := make([]interface{}, 0, 10)

	// Build the WHERE clause based on the cursor and filters
	if incomingCursor != nil {
		whereClause := ""

		// Add cursor-based pagination constraints
		if incomingCursor.IsNext {
			// Next page (older entries) - create_time DESC order
			whereClause = "(create_time, console_username, action, resource, id) < (?, ?, ?, ?, ?)"
			order = " ORDER BY create_time DESC, console_username DESC, action DESC, resource DESC, id DESC"
		} else {
			// Previous page (newer entries) - create_time ASC order
			whereClause = "(create_time, console_username, action, resource, id) > (?, ?, ?, ?, ?)"
			order = " ORDER BY create_time ASC, console_username ASC, action ASC, resource ASC, id ASC"
		}

		params = append(params,
			incomingCursor.Timestamp,
			incomingCursor.Username,
			incomingCursor.Action.String(),
			incomingCursor.Resource.String(),
			incomingCursor.ID)

		// Apply filters if provided
		filterConditions := []string{whereClause}

		if incomingCursor.UsernameFilter != "" {
			filterConditions = append(filterConditions, "console_username = ?")
			params = append(params, incomingCursor.UsernameFilter)
		}

		if incomingCursor.ActionFilter != 0 {
			filterConditions = append(filterConditions, "action = ?")
			params = append(params, incomingCursor.ActionFilter.String())
		}

		if incomingCursor.ResourceFilter != nil {
			filterConditions = append(filterConditions, "resource = ?")
			params = append(params, incomingCursor.ResourceFilter.String())
		}

		if !incomingCursor.AfterFilter.IsZero() {
			filterConditions = append(filterConditions, "create_time > ?")
			params = append(params, incomingCursor.AfterFilter)
		}

		if !incomingCursor.BeforeFilter.IsZero() {
			filterConditions = append(filterConditions, "create_time < ?")
			params = append(params, incomingCursor.BeforeFilter)
		}

		query += " WHERE " + strings.Join(filterConditions, " AND ")
	} else {
		// No cursor - initial query
		filterConditions := []string{}

		if username != "" {
			filterConditions = append(filterConditions, "console_username = ?")
			params = append(params, username)
		}

		if action != 0 {
			filterConditions = append(filterConditions, "action = ?")
			params = append(params, action.String())
		}

		if resource != nil {
			filterConditions = append(filterConditions, "resource = ?")
			params = append(params, resource.String())
		}

		if !after.IsZero() {
			filterConditions = append(filterConditions, "create_time > ?")
			params = append(params, after)
		}

		if !before.IsZero() {
			filterConditions = append(filterConditions, "create_time < ?")
			params = append(params, before)
		}

		if len(filterConditions) > 0 {
			query += " WHERE " + strings.Join(filterConditions, " AND ")
		}

		// Default order for initial query
		order = " ORDER BY create_time DESC, console_username DESC, action DESC, resource DESC, id DESC"
	}

	// Add order to query
	query += order

	// Add 1 to check if there are more results
	params = append(params, limit+1)
	query += " LIMIT ?"

	rows, err := db.QueryContext(ctx, query, params...)
	if err != nil {
		logger.Error("Error retrieving audit logs", zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	var nextCursor *auditLogListCursorMySQL
	var prevCursor *auditLogListCursorMySQL
	auditLogs := make([]*console.AuditLogList_AuditLog, 0, limit)

	var id, dbUserID, dbUsername, email, metadata, message string
	var dbResource, dbAction int32
	var timestamp time.Time

	for rows.Next() {
		if len(auditLogs) >= limit {
			nextCursor = &auditLogListCursorMySQL{
				ID:                id,
				Username:          dbUsername,
				Action:            console.AuditLogAction(dbAction),
				Resource:          console.AclResources(dbResource),
				Timestamp:         timestamp,
				IsNext:            true,
				UsernameFilter:    username,
				ActionFilter:      action,
				ResourceFilter:    resource,
				ResourceFilterSet: resource != nil,
				AfterFilter:       after, BeforeFilter: before,
			}
			break
		}

		var dbResourceStr, dbActionStr string
		if err = rows.Scan(&id, &dbUserID, &dbUsername, &email, &dbResourceStr, &dbActionStr, &metadata, &message, &timestamp); err != nil {
			logger.Error("Error scanning audit log entry", zap.Error(err))
			return nil, err
		}

		dbResource = console.AclResources_value[dbResourceStr]
		dbAction = console.AuditLogAction_value[dbActionStr]

		auditLog := &console.AuditLogList_AuditLog{
			Id:        id,
			UserId:    dbUserID,
			Username:  dbUsername,
			Email:     email,
			Resource:  console.AclResources(dbResource),
			Action:    console.AuditLogAction(dbAction),
			Metadata:  metadata,
			Message:   message,
			Timestamp: timestamppb.New(timestamp),
		}

		auditLogs = append(auditLogs, auditLog)

		if incomingCursor != nil && prevCursor == nil {
			// First row becomes the previous cursor
			prevCursor = &auditLogListCursorMySQL{
				ID:                id,
				Username:          dbUsername,
				Action:            console.AuditLogAction(dbAction),
				Resource:          console.AclResources(dbResource),
				Timestamp:         timestamp,
				IsNext:            false,
				UsernameFilter:    username,
				ActionFilter:      action,
				ResourceFilter:    resource,
				ResourceFilterSet: resource != nil,
				AfterFilter:       after, BeforeFilter: before,
			}
		}
	}
	if err = rows.Err(); err != nil {
		logger.Error("Error iterating audit log rows", zap.Error(err))
		return nil, err
	}
	_ = rows.Close()

	// If we were paginating backwards (using a prev cursor), we need to reverse the results
	if incomingCursor != nil && !incomingCursor.IsNext {
		if nextCursor != nil && prevCursor != nil {
			nextCursor, nextCursor.IsNext, prevCursor, prevCursor.IsNext = prevCursor, prevCursor.IsNext, nextCursor, nextCursor.IsNext
		} else if nextCursor != nil {
			nextCursor, prevCursor = nil, nextCursor
			prevCursor.IsNext = !prevCursor.IsNext
		} else if prevCursor != nil {
			nextCursor, prevCursor = prevCursor, nil
			nextCursor.IsNext = !nextCursor.IsNext
		}

		slices.Reverse(auditLogs)
	}

	// Encode the cursors
	var nextCursorStr string
	if nextCursor != nil {
		cursorBuf := new(bytes.Buffer)
		if err := gob.NewEncoder(cursorBuf).Encode(nextCursor); err != nil {
			logger.Error("Error creating audit log list cursor", zap.Error(err))
			return nil, err
		}
		nextCursorStr = base64.URLEncoding.EncodeToString(cursorBuf.Bytes())
	}

	var prevCursorStr string
	if prevCursor != nil {
		cursorBuf := new(bytes.Buffer)
		if err := gob.NewEncoder(cursorBuf).Encode(prevCursor); err != nil {
			logger.Error("Error creating audit log list cursor", zap.Error(err))
			return nil, err
		}
		prevCursorStr = base64.URLEncoding.EncodeToString(cursorBuf.Bytes())
	}

	return &console.AuditLogList{
		Entries:    auditLogs,
		NextCursor: nextCursorStr,
		PrevCursor: prevCursorStr,
	}, nil
}
