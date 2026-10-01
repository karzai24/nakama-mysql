// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/heroiclabs/nakama/v3/social"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type providerRoundTripper func(*http.Request) (*http.Response, error)

func (f providerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Only the provider HTTP transport is replaced, in tests that never run in
// parallel. Real JWT/HMAC/Game Center signature validation remains in the path.
// Unknown requests fail closed; no test contacts a provider on the internet.
type providerHarness struct {
	key             *rsa.PrivateKey
	client          *social.Client
	config          Config
	logger          *zap.Logger
	mu              sync.RWMutex
	classic         map[string]social.FacebookProfile
	steam           map[string]string
	play            map[string]social.GooglePlayServiceProfile
	facebookFriends map[string][][]string
	steamFriends    map[string][]string
	friendErrors    map[string]int
}

func newProviderHarness(t *testing.T) *providerHarness {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "local provider fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	require.NoError(t, err)
	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	jwks := map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "fixture", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}}
	h := &providerHarness{key: key, logger: zap.NewNop(), classic: map[string]social.FacebookProfile{}, steam: map[string]string{}, play: map[string]social.GooglePlayServiceProfile{}}
	h.facebookFriends = map[string][][]string{}
	h.steamFriends = map[string][]string{}
	h.friendErrors = map[string]int{}

	h.config = NewConfig(h.logger)
	h.config.GetSocial().Apple.BundleId = "local.provider.fixture"
	h.config.GetSocial().FacebookInstantGame.AppSecret = GenerateString()
	h.config.GetSocial().Steam.PublisherKey = GenerateString()
	h.config.GetSocial().Steam.AppID = 1
	old := http.DefaultTransport
	http.DefaultTransport = providerRoundTripper(func(r *http.Request) (*http.Response, error) {
		code := http.StatusOK
		var body []byte
		encode := func(v any) { body, _ = json.Marshal(v) }
		h.mu.RLock()
		defer h.mu.RUnlock()
		switch r.URL.Host + r.URL.Path {
		case "appleid.apple.com/auth/keys", "limited.facebook.com/.well-known/oauth/openid/jwks/":
			encode(jwks)
		case "www.googleapis.com/oauth2/v1/certs":
			encode(map[string]string{"fixture": string(pemCert)})
		case "static.gc.apple.com/fixture.cer":
			body = der
		case "graph.facebook.com/v22.0/me":
			p, ok := h.classic[r.URL.Query().Get("access_token")]
			if ok {
				encode(p)
			} else {
				code = 401
				encode(map[string]string{"error": "invalid fixture token"})
			}
		case "partner.steam-api.com/ISteamUserAuth/AuthenticateUserTicket/v1/":
			id, ok := h.steam[r.URL.Query().Get("ticket")]
			if ok {
				encode(map[string]any{"response": map[string]any{"params": map[string]string{"steamid": id}}})
			} else {
				code = 401
				encode(map[string]string{"error": "invalid fixture ticket"})
			}
		case "graph.facebook.com/v22.0/me/friends":
			token := r.URL.Query().Get("access_token")
			page, _ := strconv.Atoi(r.URL.Query().Get("after"))
			pages, ok := h.facebookFriends[token]
			if !ok {
				code = 401
				encode(map[string]string{"error": "invalid fixture token"})
			} else if failure := h.friendErrors[token+":"+strconv.Itoa(page)]; failure != 0 {
				code = failure
				encode(map[string]string{"error": "fixture failure"})
			} else {
				data := []map[string]string{}
				if page < len(pages) {
					for _, id := range pages[page] {
						data = append(data, map[string]string{"id": id})
					}
				}
				paging := map[string]any{}
				if page+1 < len(pages) {
					paging = map[string]any{"next": "fixture", "cursors": map[string]string{"after": strconv.Itoa(page + 1)}}
				}
				encode(map[string]any{"data": data, "paging": paging})
			}
		case "partner.steam-api.com/ISteamUser/GetFriendList/v0001/":
			id := r.URL.Query().Get("steamid")
			if failure := h.friendErrors[id]; failure != 0 {
				code = failure
				encode(map[string]string{"error": "fixture failure"})
			} else {
				data := []map[string]string{}
				for _, friend := range h.steamFriends[id] {
					data = append(data, map[string]string{"steamid": friend})
				}
				encode(map[string]any{"friendsList": map[string]any{"friends": data}})
			}

		case "oauth.fixture.invalid/token":
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			token := r.Form.Get("code")
			if _, ok := h.play[token]; ok {
				encode(map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": 3600})
			} else {
				code = 401
				encode(map[string]string{"error": "invalid_grant"})
			}
		case "www.googleapis.com/games/v1/players/me":
			p, ok := h.play[r.URL.Query().Get("access_token")]
			if ok {
				encode(p)
			} else {
				code = 401
				encode(map[string]string{"error": "invalid fixture token"})
			}
		default:
			return nil, fmt.Errorf("unexpected provider fixture request to %s%s", r.URL.Host, r.URL.Path)
		}
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	h.client = social.NewClient(h.logger, 5*time.Second, &oauth2.Config{ClientID: "fixture", ClientSecret: GenerateString(), Endpoint: oauth2.Endpoint{TokenURL: "https://oauth.fixture.invalid/token", AuthStyle: oauth2.AuthStyleInParams}})
	return h
}

type providerProof struct {
	harness                      *providerHarness
	kind, token, salt, signature string
	timestamp                    int64
	profile                      identityProviderProfile
}

func newProviderProof(t *testing.T, h *providerHarness, kind string) providerProof {
	t.Helper()
	column := map[string]string{"apple": "apple_id", "facebook": "facebook_id", "facebook_classic": "facebook_id", "instant": "facebook_instant_game_id", "google": "google_id", "google_play": "google_id", "gamecenter": "gamecenter_id", "steam": "steam_id"}[kind]
	p := providerProof{harness: h, kind: kind, profile: identityProviderProfile{column: column, id: GenerateString()}}
	if column == "google_id" || column == "facebook_id" {
		p.profile.displayName = "Provider فارسی"
		p.profile.avatarURL = "https://example.invalid/avatar.png"
	}
	if kind == "steam" {
		var b [8]byte
		_, err := rand.Read(b[:])
		require.NoError(t, err)
		p.profile.id = strconv.FormatUint(binary.BigEndian.Uint64(b[:]), 10)
	}
	p.sign(t, time.Now().Add(time.Hour))
	return p
}

func (p *providerProof) sign(t *testing.T, expires time.Time) {
	t.Helper()
	h := p.harness
	switch p.kind {
	case "apple", "facebook", "google":
		issuer := map[string]string{"apple": "https://appleid.apple.com", "facebook": "https://www.facebook.com", "google": "https://accounts.google.com"}[p.kind]
		claims := jwt.MapClaims{"iss": issuer, "sub": p.profile.id, "aud": "local.provider.fixture", "azp": "fixture", "iat": time.Now().Unix(), "exp": expires.Unix(), "email": p.profile.email, "email_verified": true, "name": p.profile.displayName, "picture": p.profile.avatarURL}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "fixture"
		var err error
		p.token, err = token.SignedString(h.key)
		require.NoError(t, err)
	case "instant":
		payload, err := json.Marshal(map[string]any{"algorithm": "HMAC-SHA256", "player_id": p.profile.id, "issued_at": time.Now().Unix()})
		require.NoError(t, err)
		encoded := base64.RawURLEncoding.EncodeToString(payload)
		mac := hmac.New(sha256.New, []byte(h.config.GetSocial().FacebookInstantGame.AppSecret))
		_, err = mac.Write([]byte(encoded))
		require.NoError(t, err)
		p.token = base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) + "." + encoded
	case "gamecenter":
		salt := []byte(GenerateString())
		p.salt = base64.StdEncoding.EncodeToString(salt)
		p.timestamp = time.Now().UnixMilli()
		ts := make([]byte, 8)
		binary.BigEndian.PutUint64(ts, uint64(p.timestamp))
		data := bytes.Join([][]byte{[]byte(p.profile.id), []byte(h.config.GetSocial().Apple.BundleId), ts, salt}, nil)
		hash := sha256.Sum256(data)
		sig, err := rsa.SignPKCS1v15(rand.Reader, h.key, crypto.SHA256, hash[:])
		require.NoError(t, err)
		p.signature = base64.StdEncoding.EncodeToString(sig)
	case "facebook_classic":
		p.token = "classic-" + GenerateString()
		profile := social.FacebookProfile{ID: p.profile.id, Name: p.profile.displayName, Email: p.profile.email}
		profile.Picture.Data.Url = p.profile.avatarURL
		h.mu.Lock()
		h.classic[p.token] = profile
		h.mu.Unlock()
	case "steam":
		p.token = "steam-" + GenerateString()
		h.mu.Lock()
		h.steam[p.token] = p.profile.id
		h.mu.Unlock()
	case "google_play":
		p.token = "play-" + GenerateString()
		h.mu.Lock()
		h.play[p.token] = social.GooglePlayServiceProfile{PlayerId: p.profile.id, OriginalPlayerId: p.profile.originalID, DisplayName: p.profile.displayName, AvatarImageUrl: p.profile.avatarURL}
		h.mu.Unlock()
	}
}

func (p providerProof) authenticate(ctx context.Context, db *sql.DB, username string, create bool) (string, string, bool, error) {
	h := p.harness
	switch p.kind {
	case "apple":
		return AuthenticateApple(ctx, h.logger, db, h.client, h.config.GetSocial().Apple.BundleId, p.token, username, create)
	case "facebook", "facebook_classic":
		return AuthenticateFacebook(ctx, h.logger, db, h.client, "fixture", p.token, username, create)
	case "instant":
		return AuthenticateFacebookInstantGame(ctx, h.logger, db, h.client, h.config.GetSocial().FacebookInstantGame.AppSecret, p.token, username, create)
	case "google", "google_play":
		return AuthenticateGoogle(ctx, h.logger, db, h.client, p.token, username, create)
	case "gamecenter":
		return AuthenticateGameCenter(ctx, h.logger, db, h.client, p.profile.id, h.config.GetSocial().Apple.BundleId, p.timestamp, p.salt, p.signature, "https://static.gc.apple.com/fixture.cer", username, create)
	case "steam":
		id, name, steamID, created, err := AuthenticateSteam(ctx, h.logger, db, h.client, 1, h.config.GetSocial().Steam.PublisherKey, p.token, username, create)
		if err == nil && steamID != p.profile.id {
			return "", "", false, fmt.Errorf("incorrect returned Steam ID")
		}
		return id, name, created, err
	default:
		panic("unknown fixture kind")
	}
}

func (p providerProof) link(ctx context.Context, db *sql.DB, id uuid.UUID) error {
	h := p.harness
	switch p.kind {
	case "apple":
		return LinkApple(ctx, h.logger, db, h.config, h.client, id, p.token)
	case "facebook", "facebook_classic":
		return LinkFacebook(ctx, h.logger, db, h.client, nil, nil, id, "unused", "fixture", p.token, false)
	case "instant":
		return LinkFacebookInstantGame(ctx, h.logger, db, h.config, h.client, id, p.token)
	case "google", "google_play":
		return LinkGoogle(ctx, h.logger, db, h.client, id, p.token)
	case "gamecenter":
		return LinkGameCenter(ctx, h.logger, db, h.client, id, p.profile.id, h.config.GetSocial().Apple.BundleId, p.timestamp, p.salt, p.signature, "https://static.gc.apple.com/fixture.cer")
	case "steam":
		return LinkSteam(ctx, h.logger, db, h.config, h.client, nil, nil, id, "unused", p.token, false)
	default:
		panic("unknown fixture kind")
	}
}

func (p providerProof) unlink(ctx context.Context, db *sql.DB, id uuid.UUID, proof bool) error {
	h := p.harness
	if !proof {
		p.token = ""
		p.salt = ""
		p.signature = ""
		p.timestamp = 0
		p.profile.id = ""
	}
	switch p.kind {
	case "apple":
		return UnlinkApple(ctx, h.logger, db, h.config, h.client, id, p.token)
	case "facebook", "facebook_classic":
		return UnlinkFacebook(ctx, h.logger, db, h.client, "fixture", id, p.token)
	case "instant":
		return UnlinkFacebookInstantGame(ctx, h.logger, db, h.config, h.client, id, p.token)
	case "google", "google_play":
		return UnlinkGoogle(ctx, h.logger, db, h.client, id, p.token)
	case "gamecenter":
		return UnlinkGameCenter(ctx, h.logger, db, h.client, id, p.profile.id, h.config.GetSocial().Apple.BundleId, p.timestamp, p.salt, p.signature, "https://static.gc.apple.com/fixture.cer")
	case "steam":
		return UnlinkSteam(ctx, h.logger, db, h.config, h.client, id, p.token)
	default:
		panic("unknown fixture kind")
	}
}

func TestDualBackendProviderIdentityLifecycle(t *testing.T) {
	h := newProviderHarness(t)
	for _, kind := range []string{"apple", "facebook", "facebook_classic", "instant", "google", "google_play", "gamecenter", "steam"} {
		t.Run(kind, func(t *testing.T) {
			db, ctx := identityTestDB(t), context.Background()
			p := newProviderProof(t, h, kind)
			username := GenerateString()
			_, _, _, err := p.authenticate(ctx, db, username, false)
			require.Equal(t, codes.NotFound, status.Code(err))
			id, name, created, err := p.authenticate(ctx, db, username, true)
			require.NoError(t, err)
			cleanupIdentityUser(t, db, id)
			require.True(t, created)
			require.Equal(t, username, name)
			owner := uuid.Must(uuid.FromString(id))
			again, name, created, err := p.authenticate(ctx, db, GenerateString(), false)
			require.NoError(t, err)
			require.Equal(t, id, again)
			require.Equal(t, username, name)
			require.False(t, created)
			require.Equal(t, codes.PermissionDenied, status.Code(p.unlink(ctx, db, owner, true)))
			missing := newProviderProof(t, h, kind)
			_, _, _, err = missing.authenticate(ctx, db, username, true)
			require.Equal(t, codes.AlreadyExists, status.Code(err))
			setIdentityBan(t, db, id, true)
			_, _, _, err = p.authenticate(ctx, db, username, false)
			require.Equal(t, codes.PermissionDenied, status.Code(err))
			setIdentityBan(t, db, id, false)
			device := newLocalIdentity("device")
			require.NoError(t, device.link(ctx, db, owner))
			require.Equal(t, codes.PermissionDenied, status.Code(missing.unlink(ctx, db, owner, true)))
			require.NoError(t, p.unlink(ctx, db, owner, true))
			_, _, _, err = p.authenticate(ctx, db, username, false)
			require.Equal(t, codes.NotFound, status.Code(err))
			require.NoError(t, p.link(ctx, db, owner))
			require.NoError(t, p.link(ctx, db, owner))
			other := uuid.Must(uuid.NewV4())
			InsertUser(t, db, other)
			cleanupIdentityUser(t, db, other.String())
			require.Equal(t, codes.AlreadyExists, status.Code(p.link(ctx, db, other)))
			require.NoError(t, device.unlink(ctx, db, owner))
			require.Equal(t, codes.PermissionDenied, status.Code(p.unlink(ctx, db, owner, false)))
			require.NoError(t, device.link(ctx, db, owner))
			require.NoError(t, p.unlink(ctx, db, owner, false))
			_, _, _, err = p.authenticate(ctx, db, username, false)
			require.Equal(t, codes.NotFound, status.Code(err))
		})
	}
}

func TestDualBackendProviderProofRejection(t *testing.T) {
	h := newProviderHarness(t)
	for _, kind := range []string{"apple", "facebook", "facebook_classic", "instant", "google", "google_play", "gamecenter", "steam"} {
		t.Run(kind, func(t *testing.T) {
			db, ctx := identityTestDB(t), context.Background()
			p := newProviderProof(t, h, kind)
			id, _, _, err := p.authenticate(ctx, db, GenerateString(), true)
			require.NoError(t, err)
			cleanupIdentityUser(t, db, id)
			owner := uuid.Must(uuid.FromString(id))
			device := newLocalIdentity("device")
			require.NoError(t, device.link(ctx, db, owner))
			invalid := p
			invalid.token = "invalid-proof"
			if kind == "apple" || kind == "facebook" || kind == "google" || kind == "instant" {
				parts := strings.Split(p.token, ".")
				signaturePart := len(parts) - 1
				if kind == "instant" {
					signaturePart = 0
				}
				forged, err := base64.RawURLEncoding.DecodeString(parts[signaturePart])
				require.NoError(t, err)
				forged[0] ^= 0xff
				parts[signaturePart] = base64.RawURLEncoding.EncodeToString(forged)
				invalid.token = strings.Join(parts, ".")
			}
			invalid.signature = base64.StdEncoding.EncodeToString([]byte("invalid signature"))
			found, _, _, err := invalid.authenticate(ctx, db, GenerateString(), true)
			require.Error(t, err)
			require.Empty(t, found)
			require.Error(t, invalid.link(ctx, db, owner))
			require.Error(t, invalid.unlink(ctx, db, owner, true))
			again, _, _, err := p.authenticate(ctx, db, GenerateString(), false)
			require.NoError(t, err)
			require.Equal(t, id, again)
			if kind == "apple" || kind == "facebook" || kind == "google" {
				expired := p
				expired.sign(t, time.Now().Add(-time.Hour))
				_, _, _, err = expired.authenticate(ctx, db, GenerateString(), true)
				require.Equal(t, codes.Unauthenticated, status.Code(err))
			}
		})
	}
}

func TestDualBackendProviderProfileImports(t *testing.T) {
	h := newProviderHarness(t)
	for _, kind := range []string{"apple", "facebook", "facebook_classic", "google"} {
		t.Run(kind, func(t *testing.T) {
			db, ctx := identityTestDB(t), context.Background()
			p := newProviderProof(t, h, kind)
			p.profile.email = GenerateString() + "@example.invalid"
			p.sign(t, time.Now().Add(time.Hour))
			id, _, _, err := p.authenticate(ctx, db, GenerateString(), true)
			require.NoError(t, err)
			cleanupIdentityUser(t, db, id)
			owner := uuid.Must(uuid.FromString(id))
			account, err := GetAccount(ctx, logger, db, nil, owner)
			require.NoError(t, err)
			require.Equal(t, p.profile.email, account.Email)
			_, err = AuthenticateUsername(ctx, logger, db, account.User.Username, "any-password")
			require.Equal(t, codes.Unauthenticated, status.Code(err))
			require.NoError(t, UpdateAccounts(ctx, logger, db, []*accountUpdate{{userID: owner, displayName: wrapperspb.String("Player chosen"), avatarURL: wrapperspb.String("https://example.invalid/player.png")}}))
			require.NoError(t, p.link(ctx, db, owner))
			account, err = GetAccount(ctx, logger, db, nil, owner)
			require.NoError(t, err)
			require.Equal(t, "Player chosen", account.User.DisplayName)
			require.Equal(t, "https://example.invalid/player.png", account.User.AvatarUrl)
			other := uuid.Must(uuid.NewV4())
			InsertUser(t, db, other)
			cleanupIdentityUser(t, db, other.String())
			second := newProviderProof(t, h, kind)
			second.profile.email = p.profile.email
			second.sign(t, time.Now().Add(time.Hour))
			require.NoError(t, second.link(ctx, db, other), "email collision must not merge accounts or reject the provider link")
			otherAccount, err := GetAccount(ctx, logger, db, nil, other)
			require.NoError(t, err)
			require.Empty(t, otherAccount.Email)
			linked, _, _, err := second.authenticate(ctx, db, GenerateString(), false)
			require.NoError(t, err)
			require.Equal(t, other.String(), linked)
		})
	}
}

func TestDualBackendGoogleRemapAndProfileFill(t *testing.T) {
	h := newProviderHarness(t)
	db, ctx := identityTestDB(t), context.Background()
	original := newProviderProof(t, h, "google_play")
	original.profile.displayName = ""
	original.profile.avatarURL = ""
	original.sign(t, time.Now().Add(time.Hour))
	id, _, _, err := original.authenticate(ctx, db, GenerateString(), true)
	require.NoError(t, err)
	cleanupIdentityUser(t, db, id)
	owner := uuid.Must(uuid.FromString(id))
	next := newProviderProof(t, h, "google_play")
	next.profile.originalID = original.profile.id
	next.sign(t, time.Now().Add(time.Hour))
	again, _, created, err := next.authenticate(ctx, db, GenerateString(), false)
	require.NoError(t, err)
	require.Equal(t, id, again)
	require.False(t, created)
	account, err := GetAccount(ctx, logger, db, nil, owner)
	require.NoError(t, err)
	require.Equal(t, next.profile.id, account.User.GoogleId)
	require.Equal(t, next.profile.displayName, account.User.DisplayName)
	require.Equal(t, next.profile.avatarURL, account.User.AvatarUrl)
	_, _, _, err = original.authenticate(ctx, db, GenerateString(), false)
	require.Equal(t, codes.NotFound, status.Code(err))
	huge := next
	huge.profile.displayName = strings.Repeat("x", 256)
	huge.profile.avatarURL = strings.Repeat("x", 513)
	huge.sign(t, time.Now().Add(time.Hour))
	require.NoError(t, huge.link(ctx, db, owner))
	account, err = GetAccount(ctx, logger, db, nil, owner)
	require.NoError(t, err)
	require.Equal(t, next.profile.displayName, account.User.DisplayName)
	device := newLocalIdentity("device")
	require.NoError(t, device.link(ctx, db, owner))
	final := newProviderProof(t, h, "google_play")
	final.profile.originalID = next.profile.id
	final.sign(t, time.Now().Add(time.Hour))
	require.NoError(t, final.unlink(ctx, db, owner, true))
	account, err = GetAccount(ctx, logger, db, nil, owner)
	require.NoError(t, err)
	require.Empty(t, account.User.GoogleId)
}

func TestDualBackendProviderRaces(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	h := newProviderHarness(t)
	for _, kind := range []string{"apple", "facebook", "instant", "google", "gamecenter", "steam"} {
		t.Run(kind, func(t *testing.T) {
			p := newProviderProof(t, h, kind)
			type result struct {
				id      string
				created bool
				err     error
			}
			const count = 8
			results, start := make(chan result, count), make(chan struct{})
			for i := 0; i < count; i++ {
				go func() {
					<-start
					id, _, created, err := p.authenticate(ctx, db, GenerateString(), true)
					results <- result{id, created, err}
				}()
			}
			close(start)
			outcomes := make([]result, 0, count)
			for i := 0; i < count; i++ {
				outcomes = append(outcomes, <-results)
			}
			creates, owner := 0, ""
			for _, r := range outcomes {
				if r.created {
					creates++
					owner = r.id
					cleanupIdentityUser(t, db, r.id)
				}
			}
			require.Equal(t, 1, creates)
			for _, r := range outcomes {
				require.NoError(t, r.err)
				require.Equal(t, owner, r.id)
			}

			p = newProviderProof(t, h, kind)
			owners := []uuid.UUID{uuid.Must(uuid.NewV4()), uuid.Must(uuid.NewV4())}
			for _, id := range owners {
				InsertUser(t, db, id)
				cleanupIdentityUser(t, db, id.String())
			}
			type linkResult struct {
				id  uuid.UUID
				err error
			}
			links, start := make(chan linkResult, 2), make(chan struct{})
			for _, id := range owners {
				go func() { <-start; links <- linkResult{id, p.link(ctx, db, id)} }()
			}
			close(start)
			linked := []linkResult{<-links, <-links}
			successes := 0
			var winner uuid.UUID
			for _, r := range linked {
				if r.err == nil {
					successes++
					winner = r.id
				} else {
					require.Equal(t, codes.AlreadyExists, status.Code(r.err))
				}
			}
			require.Equal(t, 1, successes)
			device := newLocalIdentity("device")
			require.NoError(t, device.link(ctx, db, winner))
			errors, start := make(chan error, 2), make(chan struct{})
			go func() { <-start; errors <- p.unlink(ctx, db, winner, true) }()
			go func() { <-start; errors <- device.unlink(ctx, db, winner) }()
			close(start)
			unlinked := []error{<-errors, <-errors}
			successes = 0
			for _, err := range unlinked {
				if err == nil {
					successes++
				} else {
					require.Equal(t, codes.PermissionDenied, status.Code(err))
				}
			}
			require.Equal(t, 1, successes)
			providerID, _, _, providerErr := p.authenticate(ctx, db, "unused", false)
			deviceID, _, _, deviceErr := device.authenticate(ctx, db, "unused", false)
			require.True(t, (providerErr == nil) != (deviceErr == nil), "one login must survive the simultaneous unlink")
			if providerErr == nil {
				require.Equal(t, winner.String(), providerID)
			} else {
				require.Equal(t, codes.NotFound, status.Code(providerErr))
			}
			if deviceErr == nil {
				require.Equal(t, winner.String(), deviceID)
			} else {
				require.Equal(t, codes.NotFound, status.Code(deviceErr))
			}
		})
	}
}

func TestDualBackendGoogleRemapCollision(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	h := newProviderHarness(t)
	old, next := newProviderProof(t, h, "google_play"), newProviderProof(t, h, "google_play")
	oldID, _, _, err := old.authenticate(ctx, db, GenerateString(), true)
	require.NoError(t, err)
	cleanupIdentityUser(t, db, oldID)
	nextID, _, _, err := next.authenticate(ctx, db, GenerateString(), true)
	require.NoError(t, err)
	cleanupIdentityUser(t, db, nextID)
	next.profile.originalID = old.profile.id
	next.sign(t, time.Now().Add(time.Hour))
	require.Equal(t, codes.AlreadyExists, status.Code(next.link(ctx, db, uuid.Must(uuid.FromString(oldID)))))
	oldAccount, err := GetAccount(ctx, logger, db, nil, uuid.Must(uuid.FromString(oldID)))
	require.NoError(t, err)
	require.Equal(t, old.profile.id, oldAccount.User.GoogleId)
	nextAccount, err := GetAccount(ctx, logger, db, nil, uuid.Must(uuid.FromString(nextID)))
	require.NoError(t, err)
	require.Equal(t, next.profile.id, nextAccount.User.GoogleId)
	// A remap to an unused new ID must also roll back if linking it would steal
	// another user's provider identity.
	fresh := newProviderProof(t, h, "google_play")
	fresh.profile.originalID = old.profile.id
	fresh.sign(t, time.Now().Add(time.Hour))
	require.Equal(t, codes.AlreadyExists, status.Code(fresh.link(ctx, db, uuid.Must(uuid.FromString(nextID)))))
	oldAccount, err = GetAccount(ctx, logger, db, nil, uuid.Must(uuid.FromString(oldID)))
	require.NoError(t, err)
	require.Equal(t, old.profile.id, oldAccount.User.GoogleId)
}

func TestDualBackendProviderEmailCollisionsAndRollback(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	h := newProviderHarness(t)
	for _, kind := range []string{"apple", "facebook", "google"} {
		t.Run(kind, func(t *testing.T) {
			original := newProviderProof(t, h, kind)
			original.profile.email = GenerateString() + "@example.invalid"
			original.sign(t, time.Now().Add(time.Hour))
			owner, _, _, err := original.authenticate(ctx, db, GenerateString(), true)
			require.NoError(t, err)
			cleanupIdentityUser(t, db, owner)
			collision := newProviderProof(t, h, kind)
			collision.profile.email = original.profile.email
			collision.sign(t, time.Now().Add(time.Hour))
			other, _, created, err := collision.authenticate(ctx, db, GenerateString(), true)
			require.NoError(t, err)
			cleanupIdentityUser(t, db, other)
			require.True(t, created)
			require.NotEqual(t, owner, other)
			account, err := GetAccount(ctx, logger, db, nil, uuid.Must(uuid.FromString(other)))
			require.NoError(t, err)
			require.Empty(t, account.Email)
			broken := newProviderProof(t, h, kind)
			broken.profile.email = strings.Repeat("x", 256) + "@example.invalid"
			broken.sign(t, time.Now().Add(time.Hour))
			_, _, _, err = broken.authenticate(ctx, db, GenerateString(), true)
			require.Equal(t, codes.Internal, status.Code(err))
			var count int
			query := "SELECT COUNT(*) FROM users WHERE " + broken.profile.column + " = ?"
			require.NoError(t, db.QueryRow(query, broken.profile.id).Scan(&count))
			require.Zero(t, count, "failed email import must roll back account creation")
			require.Equal(t, codes.Internal, status.Code(broken.link(ctx, db, uuid.Must(uuid.FromString(other)))))
			stillOther, _, _, err := collision.authenticate(ctx, db, "unused", false)
			require.NoError(t, err)
			require.Equal(t, other, stillOther, "failed link must preserve the previous provider identifier")
			originalOwner := uuid.Must(uuid.FromString(owner))
			require.Equal(t, codes.PermissionDenied, status.Code(original.unlink(ctx, db, originalOwner, true)),
				"an imported email without a password is not a usable backup login")
			password := GenerateString()
			require.NoError(t, LinkEmail(ctx, logger, db, originalOwner, original.profile.email, password))
			require.NoError(t, original.unlink(ctx, db, originalOwner, true))
			emailOwner, _, _, err := AuthenticateEmail(ctx, logger, db, original.profile.email, password, "unused", false)
			require.NoError(t, err)
			require.Equal(t, owner, emailOwner)

		})
	}
}
