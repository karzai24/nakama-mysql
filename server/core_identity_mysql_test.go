// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func identityTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := NewDB(t)
	// NewDB already closes MySQL pools; also close the PostgreSQL fixture pool.

	return db
}

func cleanupIdentityUser(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	t.Cleanup(func() {
		query := "DELETE FROM users WHERE id = ?"
		_, err := db.Exec(query, id)
		require.NoError(t, err)
	})
}

func setIdentityBan(t *testing.T, db *sql.DB, id string, banned bool) {
	t.Helper()
	disabled := time.Unix(0, 0).UTC()
	if banned {
		disabled = time.Now().UTC()
	}
	query := "UPDATE users SET disable_time = ? WHERE id = ?"
	_, err := db.Exec(query, disabled, id)
	require.NoError(t, err)
}

func TestDualBackendCustomAuthentication(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	custom, username := GenerateString(), GenerateString()
	_, _, _, err := AuthenticateCustom(ctx, logger, db, custom, username, false)
	require.Equal(t, codes.NotFound, status.Code(err))
	id, name, created, err := AuthenticateCustom(ctx, logger, db, custom, username, true)
	require.NoError(t, err)
	cleanupIdentityUser(t, db, id)
	require.Equal(t, username, name)
	require.True(t, created)
	for _, create := range []bool{true, false} {
		again, name, created, err := AuthenticateCustom(ctx, logger, db, custom, GenerateString(), create)
		require.NoError(t, err)
		require.Equal(t, id, again)
		require.Equal(t, username, name)
		require.False(t, created)
	}
	_, _, _, err = AuthenticateCustom(ctx, logger, db, GenerateString(), username, true)
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	setIdentityBan(t, db, id, true)
	for _, create := range []bool{true, false} {
		_, _, _, err = AuthenticateCustom(ctx, logger, db, custom, username, create)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	}
	setIdentityBan(t, db, id, false)
	_, _, _, err = AuthenticateCustom(ctx, logger, db, custom, username, false)
	require.NoError(t, err)
}

func TestDualBackendPasswordAuthentication(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	email, username, password := GenerateString()+"@example.invalid", GenerateString(), GenerateString()
	_, _, _, err := AuthenticateEmail(ctx, logger, db, email, password, username, false)
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = AuthenticateUsername(ctx, logger, db, username, password)
	require.Equal(t, codes.NotFound, status.Code(err))
	id, name, created, err := AuthenticateEmail(ctx, logger, db, email, password, username, true)
	require.NoError(t, err)
	cleanupIdentityUser(t, db, id)
	require.Equal(t, username, name)
	require.True(t, created)
	query := "SELECT password FROM users WHERE id = ?"
	var hash []byte
	require.NoError(t, db.QueryRow(query, id).Scan(&hash))
	require.NoError(t, bcrypt.CompareHashAndPassword(hash, []byte(password)))
	cost, err := bcrypt.Cost(hash)
	require.NoError(t, err)
	require.Equal(t, bcryptHashCost, cost)
	for _, create := range []bool{false, true} {
		again, name, created, err := AuthenticateEmail(ctx, logger, db, email, password, GenerateString(), create)
		require.NoError(t, err)
		require.Equal(t, id, again)
		require.Equal(t, username, name)
		require.False(t, created)
		_, _, _, err = AuthenticateEmail(ctx, logger, db, email, "wrong-password", username, create)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	}
	again, err := AuthenticateUsername(ctx, logger, db, username, password)
	require.NoError(t, err)
	require.Equal(t, id, again)
	_, err = AuthenticateUsername(ctx, logger, db, username, "wrong-password")
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	_, _, _, err = AuthenticateEmail(ctx, logger, db, GenerateString()+"@example.invalid", password, username, true)
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	setIdentityBan(t, db, id, true)
	_, _, _, err = AuthenticateEmail(ctx, logger, db, email, password, username, false)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = AuthenticateUsername(ctx, logger, db, username, password)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	setIdentityBan(t, db, id, false)
	_, err = AuthenticateUsername(ctx, logger, db, username, password)
	require.NoError(t, err)

	// An imported email without a password cannot be claimed with create=true.
	other := uuid.Must(uuid.NewV4())
	InsertUser(t, db, other)
	cleanupIdentityUser(t, db, other.String())
	importedEmail := GenerateString() + "@example.invalid"

	{
		query = "UPDATE users SET email = ? WHERE id = ?"
	}
	_, err = db.Exec(query, importedEmail, other.String())
	require.NoError(t, err)
	_, _, _, err = AuthenticateEmail(ctx, logger, db, importedEmail, password, other.String(), true)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	_, err = AuthenticateUsername(ctx, logger, db, other.String(), password)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

type localIdentityFixture struct {
	kind, value, password string
}

func newLocalIdentity(kind string) localIdentityFixture {
	value := GenerateString()
	if kind == "email" {
		value += "@example.invalid"
	}
	return localIdentityFixture{kind, value, GenerateString()}
}

func (i localIdentityFixture) authenticate(ctx context.Context, db *sql.DB, username string, create bool) (string, string, bool, error) {
	switch i.kind {
	case "custom":
		return AuthenticateCustom(ctx, logger, db, i.value, username, create)
	case "email":
		return AuthenticateEmail(ctx, logger, db, i.value, i.password, username, create)
	default:
		return AuthenticateDevice(ctx, logger, db, i.value, username, create)
	}
}

func (i localIdentityFixture) link(ctx context.Context, db *sql.DB, owner uuid.UUID) error {
	switch i.kind {
	case "custom":
		return LinkCustom(ctx, logger, db, owner, i.value)
	case "email":
		return LinkEmail(ctx, logger, db, owner, strings.ToUpper(i.value), i.password)
	default:
		return LinkDevice(ctx, logger, db, owner, i.value)
	}
}

func (i localIdentityFixture) unlink(ctx context.Context, db *sql.DB, owner uuid.UUID) error {
	switch i.kind {
	case "custom":
		return UnlinkCustom(ctx, logger, db, owner, i.value)
	case "email":
		return UnlinkEmail(ctx, logger, db, owner, strings.ToUpper(i.value))
	default:
		return UnlinkDevice(ctx, logger, db, owner, i.value)
	}
}

func TestDualBackendLocalIdentityLifecycle(t *testing.T) {
	for _, kind := range []string{"custom", "email", "device"} {
		t.Run(kind, func(t *testing.T) {
			db, ctx := identityTestDB(t), context.Background()
			base, extra := newLocalIdentity("device"), newLocalIdentity(kind)
			username := GenerateString()
			id, _, _, err := base.authenticate(ctx, db, username, true)
			require.NoError(t, err)
			cleanupIdentityUser(t, db, id)
			owner := uuid.Must(uuid.FromString(id))
			otherIdentity := newLocalIdentity(kind)
			otherID, _, _, err := otherIdentity.authenticate(ctx, db, GenerateString(), true)
			require.NoError(t, err)
			other := uuid.Must(uuid.FromString(otherID))
			cleanupIdentityUser(t, db, otherID)
			require.NoError(t, extra.link(ctx, db, owner))
			require.NoError(t, extra.link(ctx, db, owner), "re-link is idempotent")
			linked, _, created, err := extra.authenticate(ctx, db, GenerateString(), false)
			require.NoError(t, err)
			require.Equal(t, id, linked)
			require.False(t, created)
			require.Equal(t, codes.AlreadyExists, status.Code(extra.link(ctx, db, other)))
			stillOther, _, _, err := otherIdentity.authenticate(ctx, db, "unused", false)
			require.NoError(t, err, "a conflicting link must preserve the original identity/password")
			require.Equal(t, otherID, stillOther)
			require.Equal(t, codes.PermissionDenied, status.Code(extra.unlink(ctx, db, other)))
			wrong := extra
			wrong.value = "different-" + extra.value
			require.Equal(t, codes.PermissionDenied, status.Code(wrong.unlink(ctx, db, owner)))
			account, err := GetAccount(ctx, logger, db, nil, owner)
			require.NoError(t, err)
			switch kind {
			case "custom":
				require.Equal(t, extra.value, account.CustomId)
			case "email":
				require.Equal(t, extra.value, account.Email)
			case "device":
				require.Len(t, account.Devices, 2)
			}

			// Remove the original login; the new identifier must keep the account
			// accessible, and it cannot now be removed as the last identifier.
			require.NoError(t, base.unlink(ctx, db, owner))
			require.Equal(t, codes.PermissionDenied, status.Code(extra.unlink(ctx, db, owner)))
			linked, _, _, err = extra.authenticate(ctx, db, username, false)
			require.NoError(t, err)
			require.Equal(t, id, linked)
			require.NoError(t, base.link(ctx, db, owner))
			unlink := extra
			if kind != "device" {
				unlink.value = "" // Runtime callers may unlink without supplying the old value.
			}
			require.NoError(t, unlink.unlink(ctx, db, owner))
			_, _, _, err = extra.authenticate(ctx, db, username, false)
			require.Equal(t, codes.NotFound, status.Code(err))
			account, err = GetAccount(ctx, logger, db, nil, owner)
			require.NoError(t, err)
			require.Empty(t, account.Email)
			require.Empty(t, account.CustomId)
			require.Len(t, account.Devices, 1)
			if kind == "email" {
				_, err = AuthenticateUsername(ctx, logger, db, username, extra.password)
				require.Equal(t, codes.Unauthenticated, status.Code(err), "unlink clears the password as well as email")
			}
		})
	}
}

func TestDualBackendEmailRelinkAndValidation(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	base := newLocalIdentity("device")
	username := GenerateString()
	id, _, _, err := base.authenticate(ctx, db, username, true)
	require.NoError(t, err)
	cleanupIdentityUser(t, db, id)
	owner := uuid.Must(uuid.FromString(id))
	for _, err := range []error{
		LinkCustom(ctx, logger, db, owner, "short"),
		LinkCustom(ctx, logger, db, owner, "invalid custom id"),
		LinkDevice(ctx, logger, db, owner, "short"),
		LinkDevice(ctx, logger, db, owner, "invalid device id"),
		LinkEmail(ctx, logger, db, owner, "not-an-email", "valid-password"),
		LinkEmail(ctx, logger, db, owner, "valid@example.invalid", "short"),
		UnlinkDevice(ctx, logger, db, owner, ""),
	} {
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	email := newLocalIdentity("email")
	require.NoError(t, email.link(ctx, db, owner))
	changed := email
	changed.password = GenerateString()
	require.NoError(t, changed.link(ctx, db, owner))
	_, _, _, err = email.authenticate(ctx, db, username, false)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	_, _, _, err = changed.authenticate(ctx, db, username, false)
	require.NoError(t, err)
	{
		err = LinkEmail(ctx, logger, db, owner, email.value, strings.Repeat("x", 73))
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		_, _, _, err = changed.authenticate(ctx, db, username, false)
		require.NoError(t, err, "failed hashing must preserve the previous password")
	}
}

func TestDualBackendLocalIdentityConcurrentCreate(t *testing.T) {
	for _, kind := range []string{"custom", "email", "email-different-passwords"} {
		t.Run(kind, func(t *testing.T) {
			db, ctx := identityTestDB(t), context.Background()
			identityKind := kind
			if strings.HasPrefix(kind, "email") {
				identityKind = "email"
			}
			identity := newLocalIdentity(identityKind)
			n := 8
			if identityKind == "email" {
				n = 4
			}
			type result struct {
				id       string
				created  bool
				err      error
				password string
			}
			results, start := make(chan result, n), make(chan struct{})
			for i := 0; i < n; i++ {
				candidate := identity
				if kind == "email-different-passwords" {
					candidate.password = GenerateString()
				}
				go func() {
					<-start
					id, _, created, err := candidate.authenticate(ctx, db, GenerateString(), true)
					results <- result{id, created, err, candidate.password}
				}()
			}
			close(start)
			var outcomes []result
			for i := 0; i < n; i++ {
				outcomes = append(outcomes, <-results)
			}
			owner, creates, winnerPassword := "", 0, ""
			for _, r := range outcomes {
				if r.created {
					creates++
					owner, winnerPassword = r.id, r.password
					cleanupIdentityUser(t, db, r.id)
				}
			}
			require.Equal(t, 1, creates)
			for _, r := range outcomes {
				if kind == "email-different-passwords" && r.password != winnerPassword {
					require.Equal(t, codes.Unauthenticated, status.Code(r.err))
					require.Empty(t, r.id)
				} else {
					require.NoError(t, r.err)
					require.Equal(t, owner, r.id)
				}
			}
		})
	}
}

func TestDualBackendLocalIdentityLinkRaces(t *testing.T) {
	for _, kind := range []string{"custom", "email", "device"} {
		t.Run(kind, func(t *testing.T) {
			db, ctx := identityTestDB(t), context.Background()
			identity := newLocalIdentity(kind)
			owners := []uuid.UUID{uuid.Must(uuid.NewV4()), uuid.Must(uuid.NewV4())}
			type result struct {
				owner uuid.UUID
				err   error
			}
			results, start := make(chan result, 2), make(chan struct{})
			for _, owner := range owners {
				InsertUser(t, db, owner)
				cleanupIdentityUser(t, db, owner.String())
				go func() { <-start; results <- result{owner, identity.link(ctx, db, owner)} }()
			}
			close(start)
			outcomes := []result{<-results, <-results}
			successes := 0
			var winner uuid.UUID
			for _, r := range outcomes {
				if r.err == nil {
					successes++
					winner = r.owner
				} else {
					require.Equal(t, codes.AlreadyExists, status.Code(r.err))
				}
			}
			require.Equal(t, 1, successes)
			id, _, _, err := identity.authenticate(ctx, db, "unused", false)
			require.NoError(t, err)
			require.Equal(t, winner.String(), id)
		})
	}
}

func TestDualBackendLocalIdentityUnlinkRaces(t *testing.T) {
	for _, pair := range [][2]string{{"device", "device"}, {"custom", "device"}, {"email", "device"}, {"custom", "email"}} {
		t.Run(pair[0]+"-"+pair[1], func(t *testing.T) {
			db, ctx := identityTestDB(t), context.Background()
			for attempt := 0; attempt < 3; attempt++ {
				first, second := newLocalIdentity(pair[0]), newLocalIdentity(pair[1])
				id, _, _, err := first.authenticate(ctx, db, GenerateString(), true)
				require.NoError(t, err)
				cleanupIdentityUser(t, db, id)
				owner := uuid.Must(uuid.FromString(id))
				require.NoError(t, second.link(ctx, db, owner))
				results, start := make(chan error, 2), make(chan struct{})
				go func() { <-start; results <- first.unlink(ctx, db, owner) }()
				go func() { <-start; results <- second.unlink(ctx, db, owner) }()
				close(start)
				outcomes := []error{<-results, <-results}
				successes := 0
				for _, err := range outcomes {
					if err == nil {
						successes++
					} else {
						require.Equal(t, codes.PermissionDenied, status.Code(err))
					}
				}
				require.Equal(t, 1, successes, "concurrent unlink must leave one usable identifier")
				logins := 0
				for _, identity := range []localIdentityFixture{first, second} {
					found, _, _, err := identity.authenticate(ctx, db, "unused", false)
					if err == nil {
						logins++
						require.Equal(t, id, found)
					} else {
						require.Equal(t, codes.NotFound, status.Code(err))
					}
				}
				require.Equal(t, 1, logins)
			}
		})
	}
}
