// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dgryski/dgoogauth"
	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama/v3/console"
	"github.com/heroiclabs/nakama/v3/console/acl"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func consoleFixture(t *testing.T) (*ConsoleServer, context.Context) {
	t.Helper()
	db := identityTestDB(t)
	cfg := NewConfig(zap.NewNop())
	cfg.GetMFA().StorageEncryptionKey = "0123456789abcdef0123456789abcdef"
	sessions := NewLocalSessionCache(cfg.GetConsole().TokenExpirySec, 0)
	attempts := NewLocalLoginAttemptCache()
	t.Cleanup(sessions.Stop)
	t.Cleanup(attempts.Stop)
	s := &ConsoleServer{db: db, logger: zap.NewNop(), config: cfg, consoleSessionCache: sessions, sessionCache: sessions, loginAttemptCache: attempts, statusRegistry: groupStatus{}, tracker: &groupTracker{}, router: &groupRouter{}, streamManager: &groupManager{}, metrics: metrics, storageIndex: &walletTestIndex{}}
	ctx := context.WithValue(context.Background(), ctxConsoleUserIdKey{}, uuid.Nil)
	ctx = context.WithValue(ctx, ctxConsoleUsernameKey{}, "fixtureadmin")
	ctx = context.WithValue(ctx, ctxConsoleEmailKey{}, "fixture@example.invalid")
	ctx = context.WithValue(ctx, ctxConsoleUserAclKey{}, acl.Admin())
	return s, ctx
}
func consoleCreateUser(t *testing.T, s *ConsoleServer, ctx context.Context, mfa bool) (*console.AddUserResponse, string) {
	t.Helper()
	name := "test" + strings.ReplaceAll(uuid.Must(uuid.NewV4()).String(), "-", "")[:12]
	out, err := s.AddUser(ctx, &console.AddUserRequest{Username: name, Email: name + "@example.invalid", Acl: acl.Admin().ACL(), MfaRequired: mfa})
	require.NoError(t, err)
	t.Cleanup(func() { lifecycleExec(t, s.db, "DELETE FROM console_user WHERE id = ?", out.User.Id) })
	password := "fixture-password-" + GenerateString()
	_, err = s.AuthenticatePasswordChange(ctx, &console.AuthenticatePasswordChangeRequest{Code: out.Token, Password: password})
	require.NoError(t, err)
	return out, password
}
func TestDualBackendConsoleUserLifecycle(t *testing.T) {
	s, ctx := consoleFixture(t)
	u, pw := consoleCreateUser(t, s, ctx, false)
	_, err := s.Authenticate(ctx, &console.AuthenticateRequest{Username: u.User.Username, Password: "wrong"})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	login, err := s.Authenticate(ctx, &console.AuthenticateRequest{Username: u.User.Email, Password: pw})
	require.NoError(t, err)
	require.NotEmpty(t, login.Token)
	_, err = checkAuth(ctx, s.logger, s.db, s.config, "Bearer "+login.Token, "/nakama.console.Console/ListUsers", s.consoleSessionCache, s.loginAttemptCache)
	require.NoError(t, err)
	listed, err := s.GetUser(ctx, &console.Username{Username: u.User.Username})
	require.NoError(t, err)
	require.Equal(t, u.User.Id, listed.Id)
	_, err = s.UpdateUser(ctx, &console.UpdateUserRequest{Username: u.User.Username, Acl: map[string]*console.Permissions{"ACCOUNT": {Read: true}}})
	require.NoError(t, err)
	restricted, err := s.Authenticate(ctx, &console.AuthenticateRequest{Username: u.User.Username, Password: pw})
	require.NoError(t, err)
	_, err = checkAuth(ctx, s.logger, s.db, s.config, "Bearer "+restricted.Token, "/nakama.console.Console/DeleteAccount", s.consoleSessionCache, s.loginAttemptCache)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	{
		// Revision validation survives an empty in-memory cache and same-second updates.
		cache := NewLocalSessionCache(s.config.GetConsole().TokenExpirySec, 0)
		defer cache.Stop()
		_, err = checkAuth(ctx, s.logger, s.db, s.config, "Bearer "+login.Token, "/nakama.console.Console/ListUsers", cache, s.loginAttemptCache)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	}
	reset, err := s.ResetUserPassword(ctx, &console.Username{Username: u.User.Username})
	require.NoError(t, err)
	_, err = s.Authenticate(ctx, &console.AuthenticateRequest{Username: u.User.Username, Password: pw})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	_, err = s.AuthenticatePasswordChange(ctx, &console.AuthenticatePasswordChangeRequest{Code: reset.Code, Password: pw})
	require.NoError(t, err)
	{
		_, err = s.AuthenticatePasswordChange(ctx, &console.AuthenticatePasswordChangeRequest{Code: reset.Code, Password: pw})
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	}
	_, err = s.DeleteUser(ctx, &console.Username{Username: u.User.Username})
	require.NoError(t, err)
	_, err = s.Authenticate(ctx, &console.AuthenticateRequest{Username: u.User.Username, Password: pw})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}
func TestDualBackendConsoleMFA(t *testing.T) {
	s, ctx := consoleFixture(t)
	u, pw := consoleCreateUser(t, s, ctx, true)
	login, err := s.Authenticate(ctx, &console.AuthenticateRequest{Username: u.User.Username, Password: pw})
	require.NoError(t, err)
	require.Empty(t, login.Token)
	require.NotNil(t, login.MfaCode)
	var claims UserMFASetupToken
	require.NoError(t, parseJWTToken(s.config.GetConsole().SigningKey, login.MfaCode.Value, &claims))
	code := fmt.Sprintf("%06d", dgoogauth.ComputeCode(claims.MFASecret, time.Now().Unix()/30))
	setup, err := s.AuthenticateMFASetup(ctx, &console.AuthenticateMFASetupRequest{Mfa: login.MfaCode.Value, Code: code})
	require.NoError(t, err)
	require.NotEmpty(t, setup.RecoveryCodes)
	{
		_, err = s.AuthenticateMFASetup(ctx, &console.AuthenticateMFASetupRequest{Mfa: login.MfaCode.Value, Code: code})
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	}
	_, err = s.Authenticate(ctx, &console.AuthenticateRequest{Username: u.User.Username, Password: pw})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = s.Authenticate(ctx, &console.AuthenticateRequest{Username: u.User.Username, Password: pw, Mfa: "000000"})
	require.Error(t, err)
	good, err := s.Authenticate(ctx, &console.AuthenticateRequest{Username: u.User.Username, Password: pw, Mfa: code})
	require.NoError(t, err)
	require.NotEmpty(t, good.Token)
	recovery := setup.RecoveryCodes[0]
	{
		results := make(chan error, 2)
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			go func() {
				<-start
				_, e := s.Authenticate(ctx, &console.AuthenticateRequest{Username: u.User.Username, Password: pw, Mfa: recovery})
				results <- e
			}()
		}
		close(start)
		success := 0
		for i := 0; i < 2; i++ {
			if <-results == nil {
				success++
			}
		}
		require.Equal(t, 1, success)
	}
	_, err = s.Authenticate(ctx, &console.AuthenticateRequest{Username: u.User.Username, Password: pw, Mfa: recovery})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	_, err = s.ResetUserMfa(ctx, &console.ResetUserMfaRequest{Username: u.User.Username})
	require.NoError(t, err)
	login, err = s.Authenticate(ctx, &console.AuthenticateRequest{Username: u.User.Username, Password: pw})
	require.NoError(t, err)
	require.Empty(t, login.Token)
	require.NotNil(t, login.MfaCode)
	_, err = s.RequireUserMfa(ctx, &console.RequireUserMfaRequest{Username: u.User.Username, Required: false})
	require.NoError(t, err)
	login, err = s.Authenticate(ctx, &console.AuthenticateRequest{Username: u.User.Username, Password: pw})
	require.NoError(t, err)
	require.NotEmpty(t, login.Token)
}
func TestDualBackendConsoleTemplatesSettingsAndConfig(t *testing.T) {
	s, ctx := consoleFixture(t)
	name := "acl_" + GenerateString()
	a, err := s.AddAclTemplate(ctx, &console.AddAclTemplateRequest{Name: name, Description: "test", Acl: acl.Admin().ACL()})
	require.NoError(t, err)
	require.NotEmpty(t, a.Id)
	t.Cleanup(func() { lifecycleExec(t, s.db, "DELETE FROM console_acl_template WHERE id = ?", a.Id) })
	_, err = s.UpdateAclTemplate(ctx, &console.UpdateAclTemplateRequest{Id: a.Id, Name: name, Description: "changed", Acl: map[string]*console.Permissions{"ACCOUNT": {Read: true}}})
	require.NoError(t, err)
	list, err := s.ListAclTemplates(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	found := false
	for _, x := range list.Templates {
		if x.Id == a.Id {
			found = true
			require.Equal(t, "changed", x.Description)
			require.False(t, acl.New(x.Acl).IsAdmin())
		}
	}
	require.True(t, found)
	original, err := s.GetSetting(ctx, &console.SettingRequest{Name: "utc_toggle"})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, e := s.UpdateSetting(ctx, &console.UpdateSettingRequest{Name: "utc_toggle", Value: original.Value})
		require.NoError(t, e)
	})
	_, err = s.UpdateSetting(ctx, &console.UpdateSettingRequest{Name: "utc_toggle", Value: "true"})
	require.NoError(t, err)
	settings, err := s.ListSettings(ctx, &console.ListSettingsRequest{Names: []string{"utc_toggle", "missing"}})
	require.NoError(t, err)
	require.Len(t, settings.Settings, 1)
	require.Equal(t, "true", settings.Settings[0].Value)
	_, err = s.UpdateSetting(ctx, &console.UpdateSettingRequest{Name: "invalid", Value: "true"})
	require.ErrorIs(t, err, ErrSettingInvalid)
	s.config.GetDatabase().Addresses = []string{"mysql://someone:secret%40encoded@localhost:3306/db?tls=true"}
	cfg, err := s.GetConfig(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	require.NotContains(t, cfg.Config, "secret%40encoded")
	require.NotContains(t, cfg.Config, "secret@encoded")
	require.NotContains(t, cfg.Config, "0123456789abcdef")
}
func TestDualBackendConsoleAccountManagement(t *testing.T) {
	s, ctx := consoleFixture(t)
	owner, _ := lifecycleUser(t, s.db)
	other, _ := lifecycleUser(t, s.db)
	device := GenerateString()
	require.NoError(t, LinkDevice(ctx, s.logger, s.db, owner, device))
	_, err := s.UpdateAccount(ctx, &console.UpdateAccountRequest{Id: owner.String(), DisplayName: wrapperspb.String("فارسی <player>"), Metadata: wrapperspb.String(`{"large":9007199254740993}`), Wallet: wrapperspb.String(`{"coins":30}`), DeviceIds: map[string]string{device: device + "new"}})
	require.NoError(t, err)
	list, err := s.ListAccounts(ctx, &console.ListAccountsRequest{Filter: device + "new"})
	require.NoError(t, err)
	require.Len(t, list.Users, 1)
	require.Equal(t, owner.String(), list.Users[0].Id)
	require.Contains(t, list.Users[0].DisplayName, "&lt;")
	list, err = s.ListAccounts(ctx, &console.ListAccountsRequest{Filter: "فارسی"})
	require.NoError(t, err)
	found := false
	for _, u := range list.Users {
		if u.Id == owner.String() {
			found = true
		}
	}
	require.True(t, found)
	_, err = s.UpdateAccount(ctx, &console.UpdateAccountRequest{Id: owner.String(), CustomId: wrapperspb.String(""), DeviceIds: map[string]string{device + "new": ""}, Wallet: wrapperspb.String(`{"coins":999}`)})
	require.Error(t, err)
	account, err := GetAccount(ctx, s.logger, s.db, nil, owner)
	require.NoError(t, err)
	require.JSONEq(t, `{"coins":30}`, account.Wallet)
	require.Len(t, account.Devices, 1)
	var notes []*console.AccountNote
	for i := 0; i < 3; i++ {
		n, e := s.AddAccountNote(ctx, &console.AddAccountNoteRequest{AccountId: owner.String(), Note: fmt.Sprint("note", i)})
		require.NoError(t, e)
		notes = append(notes, n)
	}
	_, err = s.AddAccountNote(ctx, &console.AddAccountNoteRequest{Id: notes[0].Id, AccountId: owner.String(), Note: "changed"})
	require.NoError(t, err)
	{
		_, err = s.AddAccountNote(ctx, &console.AddAccountNoteRequest{Id: notes[0].Id, AccountId: other.String(), Note: "wrong account"})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		_, err = s.DeleteAccountNote(ctx, &console.DeleteAccountNoteRequest{NoteId: notes[0].Id, AccountId: other.String()})
		require.NoError(t, err)
		require.Equal(t, 1, lifecycleCount(t, s.db, "SELECT COUNT(*) FROM users_notes WHERE id = ?", notes[0].Id))
	}
	page, err := s.ListAccountNotes(ctx, &console.ListAccountNotesRequest{AccountId: owner.String(), Limit: 2})
	require.NoError(t, err)
	require.Len(t, page.Notes, 2)
	require.NotEmpty(t, page.Cursor)
	next, err := s.ListAccountNotes(ctx, &console.ListAccountNotesRequest{AccountId: owner.String(), Limit: 2, Cursor: page.Cursor})
	require.NoError(t, err)
	require.Len(t, next.Notes, 1)
	_, err = s.ListAccountNotes(ctx, &console.ListAccountNotesRequest{AccountId: other.String(), Limit: 2, Cursor: page.Cursor})
	require.Error(t, err)
}
func TestDualBackendConsoleStorageAndGroups(t *testing.T) {
	s, ctx := consoleFixture(t)
	owner, _ := lifecycleUser(t, s.db)
	member, _ := lifecycleUser(t, s.db)
	collection := "console_" + GenerateString()
	writes := StorageOpWrites{}
	for i := 0; i < 103; i++ {
		writes = append(writes, &StorageOpWrite{OwnerID: owner.String(), Object: &api.WriteStorageObject{Collection: collection, Key: fmt.Sprintf("key%03d", i), Value: `{"v":1}`, PermissionRead: wrapperspb.Int32(int32(i % 3))}})
	}
	_, _, err := StorageWriteObjects(ctx, s.logger, s.db, metrics, s.storageIndex, true, writes)
	require.NoError(t, err)
	for _, key := range []string{"", "key%"} {
		p, e := s.ListStorage(ctx, &console.ListStorageRequest{Collection: collection, UserId: owner.String(), Key: key})
		require.NoError(t, e)
		require.Len(t, p.Objects, 100)
		require.NotEmpty(t, p.NextCursor)
		n, e := s.ListStorage(ctx, &console.ListStorageRequest{Collection: collection, UserId: owner.String(), Key: key, Cursor: p.NextCursor})
		require.NoError(t, e)
		require.Len(t, n.Objects, 3)
		seen := map[string]bool{}
		for _, o := range append(p.Objects, n.Objects...) {
			require.False(t, seen[o.Key])
			seen[o.Key] = true
		}
	}
	gid := groupFixture(t, s.db, owner, false, 4)
	_, err = s.AddGroupUsers(ctx, &console.AddGroupUsersRequest{GroupId: gid.String(), Ids: member.String(), JoinRequest: true})
	require.NoError(t, err)
	groupEdge(t, s.db, gid, member, 3)
	_, err = s.PromoteGroupMember(ctx, &console.UpdateGroupUserStateRequest{GroupId: gid.String(), Id: member.String()})
	require.NoError(t, err)
	groupEdge(t, s.db, gid, member, 2)
	groupCount(t, s.db, gid, 2)
	_, err = s.PromoteGroupMember(ctx, &console.UpdateGroupUserStateRequest{GroupId: gid.String(), Id: member.String()})
	require.NoError(t, err)
	groupEdge(t, s.db, gid, member, 1)
	_, err = s.DemoteGroupMember(ctx, &console.UpdateGroupUserStateRequest{GroupId: gid.String(), Id: owner.String()})
	require.Error(t, err)
	groups, err := s.ListGroups(ctx, &console.ListGroupsRequest{Filter: gid.String()})
	require.NoError(t, err)
	require.Len(t, groups.Groups, 1)
	_, err = s.ExportGroup(ctx, &console.GroupId{Id: gid.String()})
	require.NoError(t, err)
}
func TestDualBackendConsoleAuditPaging(t *testing.T) {
	s, ctx := consoleFixture(t)
	name := GenerateString()
	now := time.Now().UTC().Truncate(time.Microsecond)
	resource := console.AclResources_ACCOUNT
	t.Cleanup(func() { lifecycleExec(t, s.db, "DELETE FROM console_audit_log WHERE console_username = ?", name) })
	for i := 0; i < 7; i++ {
		require.NoError(t, auditLogAdd(s.logger, s.db, &AuditLogEntry{UserID: uuid.Nil.String(), Username: name, Email: "", Resource: resource.String(), Action: console.AuditLogAction_UPDATE.String(), Metadata: `{"test":true}`, Timestamp: now.Add(time.Duration(i) * time.Microsecond)}))
	}
	after, before := now.Add(-time.Second), now.Add(time.Second)
	all, err := auditLogEntryList(ctx, s.logger, s.db, name, console.AuditLogAction_UPDATE, &resource, after, before, 100, "")
	require.NoError(t, err)
	require.Len(t, all.Entries, 7)
	p, err := auditLogEntryList(ctx, s.logger, s.db, name, console.AuditLogAction_UPDATE, &resource, after, before, 2, "")
	require.NoError(t, err)
	n, err := auditLogEntryList(ctx, s.logger, s.db, name, console.AuditLogAction_UPDATE, &resource, after, before, 2, p.NextCursor)
	require.NoError(t, err)
	b, err := auditLogEntryList(ctx, s.logger, s.db, name, console.AuditLogAction_UPDATE, &resource, after, before, 2, n.PrevCursor)
	require.NoError(t, err)
	require.Equal(t, p.Entries, b.Entries)
	_, err = auditLogEntryList(ctx, s.logger, s.db, "another", console.AuditLogAction_UPDATE, &resource, after, before, 2, p.NextCursor)
	require.Error(t, err)
}
func TestDualBackendConsoleImportRollback(t *testing.T) {
	s, ctx := consoleFixture(t)
	owner, _ := lifecycleUser(t, s.db)
	index := s.storageIndex.(*walletTestIndex)
	_, _, err := StorageWriteObjects(ctx, s.logger, s.db, metrics, index, true, StorageOpWrites{walletStorageWrite(owner, "old", `{"old":1}`, "")})
	require.NoError(t, err)
	exported, err := s.ExportAccount(ctx, &console.AccountId{Id: owner.String()})
	require.NoError(t, err)
	require.Len(t, exported.Objects, 1)
	index.writes = nil
	index.deletes = nil
	exported.Account.Wallet = `{"coins":7}`
	exported.Objects = append(exported.Objects, exported.Objects[0])
	_, err = s.ImportAccount(ctx, &console.AccountImport{Id: owner.String(), Data: exported})
	require.Error(t, err)
	require.Empty(t, index.writes)
	require.Empty(t, index.deletes)
	a, err := GetAccount(ctx, s.logger, s.db, nil, owner)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, a.Wallet)
	exported.Objects = exported.Objects[:1]
	_, err = s.ImportAccount(ctx, &console.AccountImport{Id: owner.String(), Data: exported})
	require.NoError(t, err)
	require.Len(t, index.writes, 1)
	require.Len(t, index.deletes, 1)
	exported.Account.User.Id = uuid.Must(uuid.NewV4()).String()
	exported.Account.User.Username = GenerateString()
	exported.Account.CustomId = GenerateString()
	cleanupIdentityUser(t, s.db, exported.Account.User.Id)
	imported, err := s.ImportAccountFull(ctx, &console.AccountImport{Data: exported})
	require.NoError(t, err)
	require.Equal(t, exported.Account.User.Id, imported.Account.User.Id)
}
func TestDualBackendConsoleAdminRaces(t *testing.T) {
	s, ctx := consoleFixture(t)
	name := "race" + strings.ReplaceAll(uuid.Must(uuid.NewV4()).String(), "-", "")[:12]
	t.Cleanup(func() { lifecycleExec(t, s.db, "DELETE FROM console_user WHERE username = ?", name) })
	var wg sync.WaitGroup
	results := make(chan *console.User, 6)
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, e := s.dbInsertConsoleUser(ctx, s.logger, &console.AddUserRequest{Username: name, Email: name + "@example.invalid", Acl: acl.Admin().ACL()})
			results <- u
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	id := ""
	for u := range results {
		if id == "" {
			id = u.Id
		}
		require.Equal(t, id, u.Id)
	}
	_, err := s.dbInsertConsoleUser(ctx, s.logger, &console.AddUserRequest{Username: name + "x", Email: name + "@example.invalid", Acl: acl.Admin().ACL()})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	invitation, err := s.AddUser(ctx, &console.AddUserRequest{Username: name, Email: "changed" + name + "@example.invalid", Acl: acl.Admin().ACL()})
	require.NoError(t, err)
	var claims UserInvitationClaims
	require.NoError(t, parseJWTToken(s.config.GetConsole().SigningKey, invitation.Token, &claims))
	require.Equal(t, name+"@example.invalid", invitation.User.Email)
	require.Equal(t, invitation.User.Email, claims.Email)
}

func TestDualBackendConsoleAuditPasswordRedaction(t *testing.T) {
	s, ctx := consoleFixture(t)
	owner, _ := lifecycleUser(t, s.db)
	name := "audit" + GenerateString()
	ctx = context.WithValue(ctx, ctxConsoleUsernameKey{}, name)
	t.Cleanup(func() { lifecycleExec(t, s.db, "DELETE FROM console_audit_log WHERE console_username = ?", name) })
	password := "audit-test-password-" + GenerateString()
	req := &console.UpdateAccountRequest{Id: owner.String(), Email: wrapperspb.String(name + "@example.invalid"), Password: wrapperspb.String(password)}
	_, err := consoleAuditLogInterceptor(s.logger, s.db)(ctx, req, &grpc.UnaryServerInfo{FullMethod: "/nakama.console.Console/UpdateAccount"}, func(ctx context.Context, in any) (any, error) {
		return s.UpdateAccount(ctx, in.(*console.UpdateAccountRequest))
	})
	require.NoError(t, err)
	require.Equal(t, password, req.Password.Value)
	entries, err := auditLogEntryList(ctx, s.logger, s.db, name, console.AuditLogAction_UNKNOWN, nil, time.Time{}, time.Time{}, 10, "")
	require.NoError(t, err)
	require.Len(t, entries.Entries, 1)
	require.NotContains(t, entries.Entries[0].Metadata, password)
	require.Contains(t, entries.Entries[0].Metadata, "[REDACTED]")
	_, _, _, err = AuthenticateEmail(ctx, s.logger, s.db, req.Email.Value, password, "", false)
	require.NoError(t, err)
}
