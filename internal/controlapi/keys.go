package controlapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/httpapi"
)

// API key 관리 API (D02 §20 "Key | GET/POST /keys, DELETE /keys/{id} | 생성 시 원문 1회, 삭제는 revoke", ADR 0069).
//   - 조회 keys.read(Tenant Admin), 발급·폐기 keys.manage(Tenant Admin + 15분 안의 MFA step-up, ADR 0015 §3·0066).
//     API key로는 key를 관리할 수 없다(step-up이 없다).
//   - 원문 token은 생성 응답에서 한 번만 준다. Idempotency-Key 재생 응답에는 없다(저장한 응답에 평문을 두지 않는다).
//   - 폐기는 DELETE다. grace_seconds(0–86,400)를 주면 그때부터 무효다 — 교체 때 두 key를 최대 24시간 겹친다(D04 §02).

// KeyStore는 API key 저장소다 (controldb.KeyStore).
type KeyStore interface {
	ListKeys(ctx context.Context, p authz.Principal) ([]controldb.KeyMetadata, error)
	CreateKeyIdempotent(ctx context.Context, p authz.Principal, iss authz.KeyIssuance, gen authz.GeneratedKey, name string, expiresAt time.Time,
		idem controldb.IdempotencyRequest, requestID string, respond func(controldb.KeyMetadata) (controldb.StoredResponse, error)) (controldb.StoredResponse, bool, error)
	RevokeKey(ctx context.Context, p authz.Principal, keyID string, revokeAt time.Time, requestID string) error
}

// KeyGenerator는 새 key(원문과 hash)를 만든다 (authz.KeyHasher.Generate).
type KeyGenerator func(kind authz.Kind) (authz.GeneratedKey, error)

// WarnTokenNotReplayed는 Idempotency-Key 재생 응답에 원문 token이 없다는 경고다.
const WarnTokenNotReplayed = "token_not_replayed"

const (
	// MaxKeyLifetimeDays는 key 수명 상한이다(ADR 0069). 만료 없는 key는 만들지 않는다.
	MaxKeyLifetimeDays = 365
	maxKeyEnvironments = 20
	maxKeyNameLen      = 64
	maxKeyBody         = 8 << 10
	maxGraceSeconds    = 86400
)

var keyEnvironmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// KeyItem은 응답의 key metadata다. 원문·hash는 없다.
type KeyItem struct {
	KeyID string  `json:"key_id"`
	Name  *string `json:"name"`
	// Kind는 ingest(OTLP 수집) 또는 api(조회·관리 API)다.
	Kind         string   `json:"kind"`
	Scopes       []string `json:"scopes"`
	Environments []string `json:"environments"`
	IssuedBy     string   `json:"issued_by"`
	CreatedAt    string   `json:"created_at"`
	ExpiresAt    string   `json:"expires_at"`
	RevokedAt    *string  `json:"revoked_at"`
	// TokenHint는 token 앞부분이다(`mta_<key_id>_…`). 어느 key인지 맞춰 볼 때 쓴다. 비밀이 아니다.
	TokenHint string `json:"token_hint"`
}

type keysResponse struct {
	Data       []KeyItem `json:"data"`
	NextCursor *string   `json:"next_cursor"`
	Meta       Meta      `json:"meta"`
}

type keyCreatedResponse struct {
	Data KeyItem `json:"data"`
	// Token은 생성 응답에만 있다. 다시 볼 수 없다.
	Token *string      `json:"token,omitempty"`
	Meta  MutationMeta `json:"meta"`
}

func (h *Handler) registerKeys(b httpapi.Boundary) {
	h.handle("GET /api/v1/keys", b.Handle(h.authenticated(h.listKeys)))
	h.handle("POST /api/v1/keys", b.Handle(h.authenticated(h.createKey)))
	h.handle("DELETE /api/v1/keys/{key_id}", b.Handle(h.authenticated(h.revokeKey)))
}

func (h *Handler) keysEnabled() bool { return h.cfg.Keys != nil && h.cfg.GenerateKey != nil }

func keysUnavailable() error {
	return apierr.New(apierr.NotFound, "key 관리 API를 사용할 수 없습니다")
}

func kindName(k authz.Kind) string {
	if k == authz.KindIngestKey {
		return "ingest"
	}
	return "api"
}

func keyItem(m controldb.KeyMetadata) KeyItem {
	scopes := make([]string, 0, len(m.Scopes))
	for _, a := range m.Scopes {
		scopes = append(scopes, string(a))
	}
	envs := m.Environments
	if envs == nil {
		envs = []string{}
	}
	prefix := "mta_"
	if m.Kind == authz.KindIngestKey {
		prefix = "mti_"
	}
	item := KeyItem{KeyID: m.KeyID, Name: m.Name, Kind: kindName(m.Kind), Scopes: scopes, Environments: envs, IssuedBy: m.IssuedBy,
		CreatedAt: m.CreatedAt.Format(time.RFC3339Nano), ExpiresAt: m.ExpiresAt.Format(time.RFC3339Nano), TokenHint: prefix + m.KeyID + "_…"}
	if m.RevokedAt != nil {
		t := m.RevokedAt.Format(time.RFC3339Nano)
		item.RevokedAt = &t
	}
	return item
}

// listKeys는 GET /api/v1/keys다(최신순, 최대 1,000개). 폐기·만료된 key도 보인다(언제 끝났는지 확인).
func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if !h.keysEnabled() {
		return keysUnavailable()
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	keys, err := h.cfg.Keys.ListKeys(ctx, p)
	if err != nil {
		return err
	}
	resp := keysResponse{Data: make([]KeyItem, 0, len(keys)), Meta: newMeta(r)}
	for _, k := range keys {
		resp.Data = append(resp.Data, keyItem(k))
	}
	return writeJSON(w, http.StatusOK, resp)
}

type keyRequest struct {
	Kind          string   `json:"kind"`
	Name          *string  `json:"name"`
	Scopes        []string `json:"scopes"`
	Environments  []string `json:"environments"`
	ExpiresInDays int      `json:"expires_in_days"`
}

func invalidKey(field, reason string) error {
	return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: field, Reason: reason})
}

// readKeyRequest는 본문을 검증한다. scope·environment의 조합 규칙(ingest는 environment 필수 등)은 authz.ValidateKeyIssuance가 다시 본다.
func readKeyRequest(req keyRequest) (authz.Kind, []authz.Action, []string, string, error) {
	var kind authz.Kind
	allowed := authz.APIKeyScopes
	switch req.Kind {
	case "ingest":
		kind, allowed = authz.KindIngestKey, authz.IngestScopes
	case "api":
		kind = authz.KindAPIKey
	default:
		return 0, nil, nil, "", invalidKey("kind", "must be ingest or api")
	}
	if len(req.Scopes) == 0 || len(req.Scopes) > len(allowed) {
		return 0, nil, nil, "", invalidKey("scopes", "must list 1 or more allowed scopes")
	}
	scopes := make([]authz.Action, 0, len(req.Scopes))
	for _, s := range req.Scopes {
		a := authz.Action(s)
		if !slices.Contains(allowed, a) || slices.Contains(scopes, a) {
			return 0, nil, nil, "", invalidKey("scopes", "unknown, duplicate, or not allowed for this kind: "+strconv.Quote(s))
		}
		scopes = append(scopes, a)
	}
	if len(req.Environments) > maxKeyEnvironments {
		return 0, nil, nil, "", invalidKey("environments", "at most 20")
	}
	for i, e := range req.Environments {
		if !keyEnvironmentPattern.MatchString(e) || slices.Contains(req.Environments[:i], e) {
			return 0, nil, nil, "", invalidKey("environments", "each must match [A-Za-z0-9][A-Za-z0-9._-]{0,63} and be unique")
		}
	}
	if kind == authz.KindIngestKey && len(req.Environments) == 0 {
		return 0, nil, nil, "", invalidKey("environments", "ingest keys need at least one environment (D04 §02)")
	}
	if req.ExpiresInDays < 1 || req.ExpiresInDays > MaxKeyLifetimeDays {
		return 0, nil, nil, "", invalidKey("expires_in_days", "must be within [1, 365]")
	}
	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
		if name == "" || utf8.RuneCountInString(name) > maxKeyNameLen || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			return 0, nil, nil, "", invalidKey("name", "must be 1..64 characters without control characters")
		}
	}
	return kind, scopes, req.Environments, name, nil
}

// createKey는 POST /api/v1/keys다. 201 응답에 원문 token을 한 번만 싣는다.
func (h *Handler) createKey(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if !h.keysEnabled() {
		return keysUnavailable()
	}
	if err := authz.AuthorizeTenantWide(p, authz.KeysManage); err != nil {
		return err
	}
	key := r.Header.Get("Idempotency-Key")
	if !idempotencyKeyPattern.MatchString(key) {
		return apierr.NewInvalidArgument("Idempotency-Key가 필요합니다", apierr.FieldViolation{Field: "Idempotency-Key", Reason: "required header, 1..255 visible ASCII characters"})
	}
	var req keyRequest
	body, err := readJSONBodyLimit(w, r, &req, maxKeyBody, `{"kind", "name"?, "scopes", "environments"?, "expires_in_days"}`)
	if err != nil {
		return err
	}
	kind, scopes, envs, name, err := readKeyRequest(req)
	if err != nil {
		return err
	}
	iss, err := authz.ValidateKeyIssuance(p, kind, scopes, envs)
	if err != nil {
		if errors.Is(err, authz.ErrForbidden) || errors.Is(err, authz.ErrStepUpRequired) || errors.Is(err, authz.ErrUnauthenticated) {
			return err // 발급자가 갖지 않은 scope는 403(ADR 0015 §1)
		}
		return invalidKey("scopes", err.Error())
	}
	expiresAt := h.cfg.Now().UTC().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	requestID := httpapi.RequestIDFrom(r.Context())
	idem := controldb.IdempotencyRequest{Key: key, MethodPath: "POST /api/v1/keys", RequestHash: sha256.Sum256(body)}
	// key id 충돌(8 byte 무작위)은 새 key로 몇 번 다시 시도한다
	for attempt := 0; ; attempt++ {
		gen, err := h.cfg.GenerateKey(kind)
		if err != nil {
			return err
		}
		var created *controldb.KeyMetadata
		stored, replayed, err := h.cfg.Keys.CreateKeyIdempotent(ctx, p, iss, gen, name, expiresAt, idem, requestID,
			func(m controldb.KeyMetadata) (controldb.StoredResponse, error) {
				created = &m
				// 저장하는 응답(재생용)에는 원문이 없다
				b, err := json.Marshal(keyCreatedResponse{Data: keyItem(m),
					Meta: MutationMeta{RequestID: requestID, SchemaVersion: SchemaVersion, Warnings: []string{WarnTokenNotReplayed}}})
				return controldb.StoredResponse{Status: http.StatusCreated, Body: b}, err
			})
		switch {
		case errors.Is(err, controldb.ErrKeyIDCollision) && attempt < 2:
			continue
		case errors.Is(err, controldb.ErrIdempotencyConflict):
			return apierr.New(apierr.Conflict, "같은 Idempotency-Key를 다른 요청에 쓸 수 없습니다")
		case errors.Is(err, controldb.ErrTooManyKeys):
			return apierr.New(apierr.Conflict, "살아 있는 key가 너무 많습니다(100개). 쓰지 않는 key를 폐기하세요")
		case err != nil:
			return err
		}
		w.Header().Set("Cache-Control", "no-store")
		if replayed || created == nil {
			w.Header().Set("Idempotent-Replayed", "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(stored.Status)
			_, err := w.Write(stored.Body)
			return err
		}
		token := gen.Token
		return writeJSON(w, http.StatusCreated, keyCreatedResponse{Data: keyItem(*created), Token: &token,
			Meta: MutationMeta{RequestID: requestID, SchemaVersion: SchemaVersion, Warnings: []string{}}})
	}
}

// revokeKey는 DELETE /api/v1/keys/{key_id}?grace_seconds=다. grace_seconds(기본 0, 최대 86,400) 뒤부터 무효다.
// 이미 그 시각 전에 폐기된 key는 그대로 둔다(멱등). 없거나 다른 조직의 key는 404다.
func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if !h.keysEnabled() {
		return keysUnavailable()
	}
	if err := authz.AuthorizeTenantWide(p, authz.KeysManage); err != nil {
		return err
	}
	keyID := r.PathValue("key_id")
	if !authz.ValidKeyID(keyID) {
		return invalidKey("key_id", "must be 16 lowercase hex characters")
	}
	grace := 0
	if v := r.URL.Query().Get("grace_seconds"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxGraceSeconds {
			return invalidKey("grace_seconds", "must be within [0, 86400]")
		}
		grace = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	err := h.cfg.Keys.RevokeKey(ctx, p, keyID, h.cfg.Now().UTC().Add(time.Duration(grace)*time.Second), httpapi.RequestIDFrom(r.Context()))
	if errors.Is(err, authz.ErrNotFound) {
		return apierr.New(apierr.NotFound, "key를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
	return nil
}
