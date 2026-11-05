package controlapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/httpapi"
)

// 배포 이벤트 API (D02 §14, D03 §12, ADR 0076). CI는 deployments.write scope의 API key로 쓴다.
//   - 기록한 사람은 응답에 없다(감사에만). 자유 텍스트 필드를 받지 않는다.
//   - 조회의 빈 결과는 "기록된 배포 없음"이다. 배포가 없었다는 뜻이 아니다(화면이 그렇게 말한다, 계약 6).

const (
	maxDeploymentRange    = controldb.DeploymentRetention
	defaultDeploymentSpan = 7 * 24 * time.Hour
	// 늦게 보내는 CI 재시도·backfill은 받되 오래된 사건을 지어 넣지 못하게 한다. 미래는 시계 차이만(ADR 0076 §2).
	deploymentMaxPast   = 30 * 24 * time.Hour
	deploymentMaxFuture = 5 * time.Minute
	maxDeploymentBody   = 8 << 10
	maxDeploymentName   = 255
	maxDeploymentEnv    = 64
	// deploymentRetrySeconds는 429의 Retry-After다(분당 상한).
	deploymentRetrySeconds = 60
)

var (
	// deploymentVersionPattern은 버전 글자다(ADR 0075 — 표시선·조건·버전 facet이 같은 값을 가리키게).
	deploymentVersionPattern = regexp.MustCompile(`^[A-Za-z0-9._+~-]{1,255}$`)
	deploymentCommitPattern  = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
)

// DeploymentStore는 배포 이벤트 저장소다(controldb.DeploymentStore).
type DeploymentStore interface {
	RecordDeployment(ctx context.Context, p authz.Principal, w controldb.DeploymentWrite, idem controldb.IdempotencyRequest, requestID string, now time.Time,
		respond func(d controldb.Deployment, created bool) (controldb.StoredResponse, error)) (controldb.DeploymentResult, error)
	ListDeployments(ctx context.Context, p authz.Principal, q controldb.DeploymentQuery, now time.Time) ([]controldb.Deployment, bool, error)
}

func (h *Handler) registerDeployments(b httpapi.Boundary) {
	h.handle("POST /api/v1/deployments", b.Handle(h.authenticated(h.recordDeployment)))
	h.handle("GET /api/v1/deployments", b.Handle(h.authenticated(h.listDeployments)))
}

func deploymentsUnavailable() error {
	return apierr.New(apierr.NotFound, "배포 이벤트를 사용할 수 없습니다")
}

// DeploymentItem은 응답의 배포 이벤트다. 없는 commit은 null이다.
type DeploymentItem struct {
	ID               string  `json:"id"`
	ServiceNamespace string  `json:"service_namespace"`
	ServiceName      string  `json:"service_name"`
	Environment      string  `json:"environment"`
	Version          string  `json:"version"`
	Commit           *string `json:"commit"`
	OccurredAt       string  `json:"occurred_at"`
	RecordedAt       string  `json:"recorded_at"`
	Source           string  `json:"source"`
}

func deploymentItem(d controldb.Deployment) DeploymentItem {
	it := DeploymentItem{ID: d.ID, ServiceNamespace: d.ServiceNamespace, ServiceName: d.ServiceName, Environment: d.Environment, Version: d.Version,
		OccurredAt: d.OccurredAt.Format(time.RFC3339Nano), RecordedAt: d.RecordedAt.Format(time.RFC3339Nano), Source: d.Source}
	if d.Commit != "" {
		c := d.Commit
		it.Commit = &c
	}
	return it
}

type deploymentRequest struct {
	Service *struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"service"`
	Environment string `json:"environment"`
	Version     string `json:"version"`
	// commit·timestamp는 생략할 수 있지만 빈 값·null은 생략이 아니다(OpenAPI와 같다) — 원문으로 받아 가른다
	Commit    json.RawMessage `json:"commit"`
	Timestamp json.RawMessage `json:"timestamp"`
}

func deploymentInvalid(field, reason string) error {
	return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: field, Reason: reason})
}

// validText는 길이(byte)와 표시 가능한 글자를 본다. 제어 문자와 서식 문자(bidi override 등 — 표시선·목록의 이름을 위장할 수 있다)는 거절한다.
func validText(s string, minLen, maxLen int) bool {
	if len(s) < minLen || len(s) > maxLen || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// optionalString은 생략할 수 있는 문자열 필드다. 있으면 문자열이어야 한다(null 거절).
func optionalString(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	var v *string
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		return "", false, errors.New("must be a string")
	}
	return *v, true, nil
}

// readDeployment는 본문을 검증한다. timestamp가 없으면 수신 시각이다.
func readDeployment(r *http.Request, w http.ResponseWriter, now time.Time) ([]byte, controldb.DeploymentWrite, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDeploymentBody))
	if err != nil {
		e := apierr.New(apierr.InvalidArgument, "요청 본문이 너무 큽니다")
		e.Status = http.StatusRequestEntityTooLarge
		return nil, controldb.DeploymentWrite{}, e
	}
	var req deploymentRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields() // 자유 텍스트(설명·메시지)를 조용히 버리지 않고 거절한다
	if err := dec.Decode(&req); err != nil || dec.More() {
		return nil, controldb.DeploymentWrite{}, deploymentInvalid("body", "must be one deployment (service, environment, version, commit, timestamp)")
	}
	if req.Service == nil || !validText(req.Service.Name, 1, maxDeploymentName) {
		return nil, controldb.DeploymentWrite{}, deploymentInvalid("service.name", "required, 1..255 bytes")
	}
	if !validText(req.Service.Namespace, 0, maxDeploymentName) {
		return nil, controldb.DeploymentWrite{}, deploymentInvalid("service.namespace", "at most 255 bytes")
	}
	if !validText(req.Environment, 1, maxDeploymentEnv) {
		return nil, controldb.DeploymentWrite{}, deploymentInvalid("environment", "required, 1..64 bytes")
	}
	if !deploymentVersionPattern.MatchString(req.Version) {
		return nil, controldb.DeploymentWrite{}, deploymentInvalid("version", "required, 1..255 of letters, digits and . _ + ~ -")
	}
	commit, hasCommit, err := optionalString(req.Commit)
	if err != nil || (hasCommit && !deploymentCommitPattern.MatchString(commit)) {
		return nil, controldb.DeploymentWrite{}, deploymentInvalid("commit", "must be 7..64 hex characters")
	}
	ts, hasTime, err := optionalString(req.Timestamp)
	if err != nil {
		return nil, controldb.DeploymentWrite{}, deploymentInvalid("timestamp", "must be RFC3339")
	}
	at := now
	if hasTime {
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, controldb.DeploymentWrite{}, deploymentInvalid("timestamp", "must be RFC3339")
		}
		if t.Before(now.Add(-deploymentMaxPast)) || t.After(now.Add(deploymentMaxFuture)) {
			return nil, controldb.DeploymentWrite{}, deploymentInvalid("timestamp", "must be within the last 30 days (5 minutes ahead at most)")
		}
		at = t.UTC()
	}
	return body, controldb.DeploymentWrite{
		ServiceNamespace: req.Service.Namespace, ServiceName: req.Service.Name, Environment: req.Environment, Version: req.Version,
		Commit:     strings.ToLower(commit),
		OccurredAt: at.Truncate(time.Microsecond), // PostgreSQL 정밀도. 재시도의 같은 값 비교가 어긋나지 않게
	}, nil
}

type deploymentResponse struct {
	Data DeploymentItem `json:"data"`
	Meta MutationMeta   `json:"meta"`
}

// recordDeployment는 POST /api/v1/deployments다. 판정 순서: 권한 → Idempotency-Key → 본문 → 재생 → 한도 → 쓰기(ADR 0049).
func (h *Handler) recordDeployment(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Deployments == nil {
		return deploymentsUnavailable()
	}
	if err := authz.Authorize(p, authz.DeploymentsWrite); err != nil {
		return err
	}
	key := r.Header.Get("Idempotency-Key")
	if !idempotencyKeyPattern.MatchString(key) {
		h.observeDeployment("invalid")
		return apierr.NewInvalidArgument("Idempotency-Key가 필요합니다", apierr.FieldViolation{Field: "Idempotency-Key", Reason: "required header, 1..255 visible ASCII characters"})
	}
	now := h.cfg.Now().UTC()
	body, dw, err := readDeployment(r, w, now)
	if err != nil {
		h.observeDeployment("invalid")
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	requestID := httpapi.RequestIDFrom(r.Context())
	idem := controldb.IdempotencyRequest{Key: key, MethodPath: "POST /api/v1/deployments", RequestHash: sha256.Sum256(body)}
	res, err := h.cfg.Deployments.RecordDeployment(ctx, p, dw, idem, requestID, now, func(d controldb.Deployment, created bool) (controldb.StoredResponse, error) {
		status := http.StatusOK // 같은 값의 기존 행
		if created {
			status = http.StatusCreated
		}
		b, err := json.Marshal(deploymentResponse{Data: deploymentItem(d), Meta: MutationMeta{RequestID: requestID, SchemaVersion: SchemaVersion, Warnings: []string{}}})
		return controldb.StoredResponse{Status: status, Body: b}, err
	})
	switch {
	case errors.Is(err, controldb.ErrIdempotencyConflict):
		return apierr.New(apierr.Conflict, "같은 Idempotency-Key를 다른 요청에 쓸 수 없습니다")
	case errors.Is(err, controldb.ErrDeploymentRateLimited):
		h.observeDeployment("rate_limited")
		return apierr.NewRateLimited("배포 이벤트를 너무 많이 보냈습니다", deploymentRetrySeconds)
	case err != nil:
		return err
	}
	switch {
	case res.Replayed:
		h.observeDeployment("replayed")
		w.Header().Set("Idempotent-Replayed", "true")
	case res.Created:
		h.observeDeployment("created")
	default:
		h.observeDeployment("existing")
	}
	if res.Expired > 0 && h.cfg.ObserveDeploymentsExpired != nil {
		h.cfg.ObserveDeploymentsExpired(res.Expired)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(res.Stored.Status)
	_, err = w.Write(res.Stored.Body)
	return err
}

func (h *Handler) observeDeployment(outcome string) {
	if h.cfg.ObserveDeployment != nil {
		h.cfg.ObserveDeployment(outcome)
	}
}

type deploymentListResponse struct {
	Data       []DeploymentItem `json:"data"`
	NextCursor *string          `json:"next_cursor"`
	Meta       Meta             `json:"meta"`
}

type deploymentCursor struct {
	OccurredAt string `json:"at"`
	ID         string `json:"id"`
}

// listDeployments는 GET /api/v1/deployments?service_name[&service_namespace][&environment][&from&to][&limit][&cursor]다.
// service_namespace가 없으면 모든 namespace, 빈 값이면 namespace 없는 서비스만이다.
func (h *Handler) listDeployments(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Deployments == nil {
		return deploymentsUnavailable()
	}
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return err
	}
	v := r.URL.Query()
	name := v.Get("service_name")
	if !validText(name, 1, maxDeploymentName) {
		return deploymentInvalid("service_name", "required, 1..255 bytes")
	}
	q := controldb.DeploymentQuery{ServiceName: name, Limit: defaultLimit}
	nsKey := ""
	if v.Has("service_namespace") {
		ns := v.Get("service_namespace")
		if !validText(ns, 0, maxDeploymentName) {
			return deploymentInvalid("service_namespace", "at most 255 bytes")
		}
		q.ServiceNamespace = &ns
		nsKey = "ns:" + ns
	}
	envKey := ""
	if v.Has("environment") {
		env := v.Get("environment")
		if !validText(env, 1, maxDeploymentEnv) {
			return deploymentInvalid("environment", "1..64 bytes")
		}
		q.Environment = &env
		envKey = env
	}
	now := h.cfg.Now().UTC()
	q.To = now
	if s := v.Get("to"); s != "" {
		t, err := parseTime(s, "to")
		if err != nil {
			return err
		}
		q.To = t
	}
	q.From = q.To.Add(-defaultDeploymentSpan) // from을 생략하면 to 앞 7일이다
	if s := v.Get("from"); s != "" {
		t, err := parseTime(s, "from")
		if err != nil {
			return err
		}
		q.From = t
	}
	if !q.From.Before(q.To) || q.To.Sub(q.From) > maxDeploymentRange {
		return deploymentInvalid("to", "must be after from, within 395 days")
	}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > controldb.MaxDeploymentPage {
			return deploymentInvalid("limit", "must be within [1, 500]")
		}
		q.Limit = n
	}
	// 범위를 생략했으면 다음 page도 첫 page 기준이어야 한다 — 구체 시각을 hash에 넣지 않고 cursor의 snapshot으로 다시 만든다
	rangeKey := v.Get("from") + "/" + v.Get("to")
	fp := append([]string{p.Kind().String(), p.Subject(), string(authz.TelemetryRead)}, p.Environments()...)
	binding := apicursor.Binding{
		Tenant:      p.Tenant().String(),
		Fingerprint: apicursor.Fingerprint(fp...),
		QueryHash:   apicursor.Fingerprint("deployments", strings.ToLower(name), nsKey, envKey, rangeKey),
	}
	q.RecordedBefore = now
	if c := v.Get("cursor"); c != "" {
		bad := deploymentInvalid("cursor", "invalid, expired, or not for this request")
		claims, err := h.cfg.Cursor.Decode(c, binding)
		if err != nil {
			return bad
		}
		var pos deploymentCursor
		if json.Unmarshal(claims.Position, &pos) != nil || pos.ID == "" {
			return bad
		}
		at, err := time.Parse(time.RFC3339Nano, pos.OccurredAt)
		if err != nil {
			return bad
		}
		q.After = &controldb.DeploymentPosition{OccurredAt: at, ID: pos.ID}
		q.RecordedBefore = claims.Snapshot
		if v.Get("to") == "" {
			q.To = claims.Snapshot
		}
		if v.Get("from") == "" {
			q.From = q.To.Add(-defaultDeploymentSpan)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	rows, more, err := h.cfg.Deployments.ListDeployments(ctx, p, q, now)
	if err != nil {
		return err
	}
	resp := deploymentListResponse{Data: make([]DeploymentItem, 0, len(rows)), Meta: newMeta(r)}
	for _, d := range rows {
		resp.Data = append(resp.Data, deploymentItem(d))
	}
	if more && len(rows) > 0 {
		last := rows[len(rows)-1]
		tok, err := h.cfg.Cursor.Encode(binding, q.RecordedBefore, deploymentCursor{OccurredAt: last.OccurredAt.Format(time.RFC3339Nano), ID: last.ID})
		if err != nil {
			return err
		}
		resp.NextCursor = &tok
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	return json.NewEncoder(w).Encode(resp)
}
