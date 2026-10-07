package query

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

// ServiceStore는 서비스 catalog 저장소다 (controldb.ServiceStore).
type ServiceStore interface {
	ListServices(ctx context.Context, p authz.Principal, q controldb.ServiceQuery) ([]controldb.Service, bool, error)
	GetService(ctx context.Context, p authz.Principal, serviceID string, now time.Time) (controldb.Service, bool, error)
	// ResolveServiceNames와 EnvironmentServiceIDs는 log 검색의 service.name과 environment 범위다(ADR 0039).
	ResolveServiceNames(ctx context.Context, p authz.Principal, names []string) (map[string][]string, error)
	EnvironmentServiceIDs(ctx context.Context, p authz.Principal) ([]string, error)
	// ServiceNames는 service_id → 이름이다(trace 검색의 root 서비스, ADR 0043). 볼 수 없거나 없는 id는 결과에 없다.
	ServiceNames(ctx context.Context, p authz.Principal, ids []string) (map[string]string, error)
}

// ServiceItem은 GET /api/v1/services의 항목이다 (D02 §08 catalog 필드). 모르는 값은 null이다.
type ServiceItem struct {
	ServiceID     string   `json:"service_id"`
	Name          string   `json:"name"`
	Namespace     string   `json:"namespace"`
	Environment   string   `json:"environment"`
	Language      *string  `json:"language"`
	Status        string   `json:"status"` // active | inactive(24시간 미관측) | archived(30일)
	FirstSeen     string   `json:"first_seen"`
	LastSeen      string   `json:"last_seen"`
	OwnerTeam     *string  `json:"owner_team"`
	RepositoryURL *string  `json:"repository_url"`
	RunbookURL    *string  `json:"runbook_url"`
	Tier          *string  `json:"tier"`
	Tags          []string `json:"tags"`
}

type serviceResponse struct {
	Data ServiceItem `json:"data"`
	Meta Meta        `json:"meta"`
}

type servicesResponse struct {
	Data       []ServiceItem `json:"data"`
	NextCursor *string       `json:"next_cursor"`
	Meta       Meta          `json:"meta"`
}

type servicePosition struct {
	Name string `json:"n"`
	Env  string `json:"e"`
	ID   string `json:"i"`
}

// listServices는 GET /api/v1/services?environment&owner&include_archived&limit&cursor다 (D02 §13).
// environment로 제한된 key는 허용된 environment의 서비스만 본다(저장소의 mandatory predicate).
func (h *Handler) listServices(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Services == nil || h.cfg.Cursor == nil {
		return apierr.New(apierr.NotFound, "서비스 목록을 사용할 수 없습니다")
	}
	q := r.URL.Query()
	env, owner := q.Get("environment"), q.Get("owner")
	if len(env) > 255 || len(owner) > 255 {
		return invalid("environment", "environment and owner are at most 255 characters")
	}
	includeArchived := false
	if v := q.Get("include_archived"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return invalid("include_archived", "must be true or false")
		}
		includeArchived = b
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > controldb.MaxServicePage {
			return invalid("limit", "must be within [1, 1000]")
		}
		limit = n
	}
	fp := append([]string{p.Kind().String(), p.Subject(), string(authz.TelemetryRead)}, p.Environments()...)
	binding := apicursor.Binding{
		Tenant:      p.Tenant().String(),
		Fingerprint: apicursor.Fingerprint(fp...),
		QueryHash:   apicursor.Fingerprint("services", env, owner, strconv.FormatBool(includeArchived)),
	}
	now := h.cfg.Now().UTC()
	sq := controldb.ServiceQuery{Environment: env, OwnerTeam: owner, IncludeArchived: includeArchived, Limit: limit, Now: now}
	if c := q.Get("cursor"); c != "" {
		claims, err := h.cfg.Cursor.Decode(c, binding)
		var pos servicePosition
		if err != nil || json.Unmarshal(claims.Position, &pos) != nil || pos.ID == "" {
			return invalid("cursor", "invalid, expired, or not for this query")
		}
		sq.After = &controldb.ServicePosition{NameNormalized: pos.Name, Environment: pos.Env, ServiceID: pos.ID}
		sq.Now = claims.Snapshot // 상태(active·inactive) 판정 시각을 page 사이에 고정한다
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.QueryTimeout)
	defer cancel()
	services, more, err := h.cfg.Services.ListServices(ctx, p, sq)
	if err != nil {
		return err
	}
	resp := servicesResponse{Data: make([]ServiceItem, 0, len(services)), Meta: newMeta(r)}
	for _, s := range services {
		resp.Data = append(resp.Data, serviceItem(s))
	}
	if more && len(services) > 0 {
		last := services[len(services)-1]
		tok, err := h.cfg.Cursor.Encode(binding, sq.Now, servicePosition{Name: last.NameNormalized, Env: last.Environment, ID: last.ServiceID})
		if err != nil {
			return err
		}
		resp.NextCursor = &tok
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	return json.NewEncoder(w).Encode(resp)
}

func serviceItem(s controldb.Service) ServiceItem {
	return ServiceItem{
		ServiceID: s.ServiceID, Name: s.Name, Namespace: s.Namespace, Environment: s.Environment, Language: s.Language,
		Status: s.Status, FirstSeen: s.FirstSeen.Format(time.RFC3339Nano), LastSeen: s.LastSeen.Format(time.RFC3339Nano),
		OwnerTeam: s.OwnerTeam, RepositoryURL: s.RepositoryURL, RunbookURL: s.RunbookURL, Tier: s.Tier, Tags: s.Tags,
	}
}

// serviceIDPattern은 catalog service_id(결정적 UUID, 소문자 정규형)다.
var serviceIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// getService는 GET /api/v1/services/{service_id}다 (D05 §05 서비스 상세 metadata).
// 없는 서비스, 다른 tenant의 서비스, environment 제한 key가 볼 수 없는 서비스는 모두 같은 404다(D02 §19).
// 형식이 틀린 id는 존재와 무관하므로 400이다. archived 서비스도 돌려준다.
func (h *Handler) getService(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Services == nil {
		return apierr.New(apierr.NotFound, "서비스를 찾을 수 없습니다")
	}
	id := r.PathValue("service_id")
	if !serviceIDPattern.MatchString(id) {
		return invalid("service_id", "must be a lowercase UUID")
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.QueryTimeout)
	defer cancel()
	s, ok, err := h.cfg.Services.GetService(ctx, p, id, h.cfg.Now().UTC())
	if err != nil {
		return err
	}
	if !ok {
		return apierr.New(apierr.NotFound, "서비스를 찾을 수 없습니다")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	return json.NewEncoder(w).Encode(serviceResponse{Data: serviceItem(s), Meta: newMeta(r)})
}
