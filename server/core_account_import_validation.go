// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"encoding/json"
	"strings"

	"github.com/heroiclabs/nakama/v3/console"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Prepare an independent copy so normalizing blank JSON does not change the
// caller's export or expose partial results when a transaction fails.
func prepareAccountImport(data *console.AccountExport, create bool) (*console.AccountExport, error) {
	if data == nil || data.Account == nil || data.Account.User == nil {
		return nil, status.Error(codes.InvalidArgument, "An account is required.")
	}
	data = proto.Clone(data).(*console.AccountExport)
	objectJSON := func(value string) (string, error) {
		if strings.TrimSpace(value) == "" {
			return "{}", nil
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(value), &object); err != nil || object == nil {
			return "", status.Error(codes.InvalidArgument, "Expected a JSON object.")
		}
		return value, nil
	}
	var err error
	data.Account.User.Metadata, err = objectJSON(data.Account.User.Metadata)
	if err != nil {
		return nil, err
	}
	data.Account.Wallet, err = objectJSON(data.Account.Wallet)
	if err != nil {
		return nil, err
	}
	var balances map[string]int64
	if err = json.Unmarshal([]byte(data.Account.Wallet), &balances); err != nil {
		return nil, status.Error(codes.InvalidArgument, "Wallet values must be integers.")
	}
	for _, value := range balances {
		if value < 0 {
			return nil, status.Error(codes.InvalidArgument, "Wallet cannot be negative.")
		}
	}
	if create {
		if data.Account.User.Username == "" || invalidUsernameRegex.MatchString(data.Account.User.Username) {
			return nil, status.Error(codes.InvalidArgument, "Invalid username.")
		}
		for _, device := range data.Account.Devices {
			if device == nil || device.Id == "" {
				return nil, status.Error(codes.InvalidArgument, "Invalid device ID.")
			}
		}
	}
	for _, object := range data.Objects {
		if object == nil || object.Collection == "" || object.Key == "" || object.Version == "" || object.PermissionRead < 0 || object.PermissionRead > 2 || object.PermissionWrite < 0 || object.PermissionWrite > 1 {
			return nil, status.Error(codes.InvalidArgument, "Invalid imported storage object.")
		}
		if _, err = objectJSON(object.Value); err != nil {
			return nil, err
		}
		if object.CreateTime == nil || object.UpdateTime == nil || object.CreateTime.CheckValid() != nil || object.UpdateTime.CheckValid() != nil {
			return nil, status.Error(codes.InvalidArgument, "Invalid storage timestamp.")
		}
	}
	return data, nil
}
