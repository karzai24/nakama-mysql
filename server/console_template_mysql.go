// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama/v3/console"
	"github.com/heroiclabs/nakama/v3/console/acl"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *ConsoleServer) writeAclTemplateMySQL(ctx context.Context, id, name, description string, permissions map[string]*console.Permissions, create bool) (*console.AclTemplate, error) {
	uid, err := uuid.FromString(id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Invalid template ID.")
	}
	if name == "" || utf8.RuneCountInString(name) > 64 || utf8.RuneCountInString(description) > 64 {
		return nil, status.Error(codes.InvalidArgument, "Template name and description must be at most 64 characters, with a nonempty name.")
	}
	p := acl.New(permissions)
	value, err := p.ToJson()
	if err != nil {
		return nil, err
	}
	out := &console.AclTemplate{Id: uid.String()}
	err = executeInTxMySQL(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		if create {
			_, err = tx.ExecContext(ctx, "INSERT INTO console_acl_template (id,name,description,acl) VALUES (?,?,?,?)", uid, name, description, value)
		} else {
			_, err = tx.ExecContext(ctx, "UPDATE console_acl_template SET name = ?, description = ?, acl = ?, update_time = CURRENT_TIMESTAMP(6) WHERE id = ?", name, description, value, uid)
		}
		if err != nil {
			return err
		}
		var created, updated time.Time
		var stored string
		if err = tx.QueryRowContext(ctx, "SELECT name,description,acl,create_time,update_time FROM console_acl_template WHERE id = ?", uid).Scan(&out.Name, &out.Description, &stored, &created, &updated); err != nil {
			return err
		}
		parsed, err := acl.NewFromJson(stored)
		if err != nil {
			return err
		}
		out.Acl = parsed.ACL()
		out.CreateTime = timestamppb.New(created)
		out.UpdateTime = timestamppb.New(updated)
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "Template not found.")
	}
	if mysqlErrorNumber(err, 1062) {
		return nil, status.Error(codes.AlreadyExists, "Template name already exists.")
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}
