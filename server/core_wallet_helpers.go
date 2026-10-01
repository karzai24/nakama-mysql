// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/heroiclabs/nakama-common/runtime"
)

var errWalletOverflow = errors.New("wallet balance exceeds int64 maximum")

// Wallets have the same nonnegative int64 contract on every database.
func decodeWallet(data string) (map[string]int64, error) {
	var wallet map[string]int64
	if err := json.Unmarshal([]byte(data), &wallet); err != nil {
		return nil, err
	}
	if wallet == nil {
		return nil, errors.New("wallet must be a JSON object")
	}
	for _, value := range wallet {
		if value < 0 {
			return nil, errors.New("wallet contains a negative balance")
		}
	}
	return wallet, nil
}

func copyWallet(wallet map[string]int64) map[string]int64 {
	copied := make(map[string]int64, len(wallet))
	for key, value := range wallet {
		copied[key] = value
	}
	return copied
}

// The caller owns this decoded wallet inside a transaction. Apply in deterministic
// key order, and return independent snapshots even for repeated owners in a batch.
func applyWalletChanges(owner string, wallet, changes map[string]int64) (*runtime.WalletUpdateResult, error) {
	previous := copyWallet(wallet)
	keys := make([]string, 0, len(changes))
	for key := range changes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		amount, current := changes[key], wallet[key]
		if amount > 0 && current > math.MaxInt64-amount {
			return nil, fmt.Errorf("%w: %s", errWalletOverflow, key)
		}
		next := current + amount
		if next < 0 {
			return nil, &runtime.WalletNegativeError{UserID: owner, Path: key, Current: current, Amount: amount}
		}
		wallet[key] = next
	}
	return &runtime.WalletUpdateResult{UserID: owner, Previous: previous, Updated: copyWallet(wallet)}, nil
}
