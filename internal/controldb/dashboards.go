package controldb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

// dashboard 저장소 (D02 §11·§14, D05 §10~11, ADR 0057).
//   - 정의는 정규형 DashboardSpec(internal/dashboard)이다. 낙관적 잠금(revision, If-Match)과 tombstone 삭제.
//   - 생성은 Idempotency-Key(monitor와 같은 표·의미), 변경은 operations 감사·outbox와 한 트랜잭션.
//   - 조회는 dashboards.read(Viewer 이상), 쓰기는 dashboards.write(Developer 이상)다(D04 §01).
//     정의에는 telemetry가 없고 query는 보는 사람의 권한으로 실행되므로 environment 제한 key도 다룰 수 있다.

// MaxDashboardPage는 목록 page 상한이다.
const MaxDashboardPage = 200

// Dashboard는 저장된 dashboard다. Spec은 정규형 JSON이다.
type Dashboard struct {
	ID          string
	Title       string
	Spec        []byte
	WidgetCount int
	Revision    int64
	CreatedBy   string
	UpdatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// DashboardWrite는 저장할 정의다. Spec은 dashboard.Normalize의 정규형이어야 한다.
type DashboardWrite struct {
	Title       string
	Spec        []byte
	WidgetCount int
}

// DashboardPosition은 목록 keyset 위치다(제목 정규형, id).
type DashboardPosition struct {
	TitleNormalized string
	ID              string
}

// DashboardStore는 dashboard 저장소다.
type DashboardStore struct{ db *DB }

// NewDashboardStore는 저장소를 만든다.
func NewDashboardStore(db *DB) *DashboardStore { return &DashboardStore{db: db} }

const dashboardColumns = `id::text, title, spec, widget_count, revision, created_by, updated_by, created_at, updated_at`

func dashboardDest(d *Dashboard) []any {
	return []any{&d.ID, &d.Title, &d.Spec, &d.WidgetCount, &d.Revision, &d.CreatedBy, &d.UpdatedBy, &d.CreatedAt, &d.UpdatedAt}
}

func scanDashboard(row pgx.Row) (Dashboard, error) {
	var d Dashboard
	if err := row.Scan(dashboardDest(&d)...); err != nil {
		return Dashboard{}, err
	}
	d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
	return d, nil
}

// dashboardDetails는 감사 details다. spec 본문(제목·filter 값·text는 자유 입력)은 넣지 않고 hash와 위젯 수만 둔다.
func dashboardDetails(revision int64, w DashboardWrite, deleted bool) ([]byte, error) {
	sum := sha256.Sum256(w.Spec)
	b, err := json.Marshal(map[string]any{"revision": revision, "deleted": deleted, "widget_count": w.WidgetCount, "spec_sha256": hex.EncodeToString(sum[:])})
	if err != nil {
		return nil, fmt.Errorf("controldb: dashboard audit details: %w", err)
	}
	return b, nil
}

// CreateDashboard는 dashboard를 만든다. Idempotency-Key 의미는 CreateMonitor와 같다(같은 본문 재생, 다른 본문 409).
func (s *DashboardStore) CreateDashboard(ctx context.Context, p authz.Principal, w DashboardWrite, idem IdempotencyRequest, requestID string,
	respond func(Dashboard) (StoredResponse, error)) (StoredResponse, bool, error) {
	if err := authz.Authorize(p, authz.DashboardsWrite); err != nil {
		return StoredResponse{}, false, err
	}
	if idem.Key == "" || idem.MethodPath == "" {
		return StoredResponse{}, false, errors.New("controldb: idempotency key is required")
	}
	for attempt := 0; ; attempt++ {
		var (
			resp     StoredResponse
			replayed bool
		)
		err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
			stored, found, err := lookupIdempotency(ctx, tx, p, idem)
			if err != nil {
				return err
			}
			if found {
				resp, replayed = stored, true
				return nil
			}
			id, err := newUUID()
			if err != nil {
				return err
			}
			d, err := scanDashboard(tx.QueryRow(ctx, `
				INSERT INTO dashboards (tenant_id, id, title, spec, widget_count, created_by, updated_by)
				VALUES (app_tenant_id(), $1, $2, $3, $4, $5, $5)
				RETURNING `+dashboardColumns, id, w.Title, w.Spec, w.WidgetCount, p.Subject()))
			if err != nil {
				return classify("insert dashboard", err)
			}
			details, err := dashboardDetails(d.Revision, w, false)
			if err != nil {
				return err
			}
			if err := writeAuditAndOutbox(ctx, tx, p.Tenant(), change{
				category: "operations", action: "dashboard.created", actorKind: p.Kind().String(), actorID: p.Subject(),
				resourceType: "dashboard", resourceID: d.ID, revision: d.Revision, requestID: requestID, details: details,
			}); err != nil {
				return err
			}
			if resp, err = respond(d); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO idempotency_keys (tenant_id, principal, method_path, key, request_hash, status_code, response_body, expires_at)
				VALUES (app_tenant_id(), $1, $2, $3, $4, $5, $6, now() + $7::interval)`,
				principalKey(p), idem.MethodPath, idem.Key, idem.RequestHash[:], resp.Status, resp.Body, fmt.Sprintf("%d seconds", int(IdempotencyTTL/time.Second)))
			if isUniqueViolation(err) {
				return errIdempotencyRace
			}
			return classify("insert idempotency key", err)
		})
		if errors.Is(err, errIdempotencyRace) {
			if attempt < maxIdempotencyAttempts-1 {
				continue
			}
			return StoredResponse{}, false, &unavailableError{op: "idempotent create", err: err}
		}
		if err != nil {
			return StoredResponse{}, false, err
		}
		return resp, replayed, nil
	}
}

// GetDashboard는 삭제되지 않은 dashboard 하나다. 없거나 다른 tenant거나 삭제됐으면 authz.ErrNotFound다(존재를 숨긴다).
func (s *DashboardStore) GetDashboard(ctx context.Context, p authz.Principal, id string) (Dashboard, error) {
	if err := authz.Authorize(p, authz.DashboardsRead); err != nil {
		return Dashboard{}, err
	}
	var d Dashboard
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var err error
		d, err = scanDashboard(tx.QueryRow(ctx, `SELECT `+dashboardColumns+` FROM dashboards WHERE id = $1 AND deleted_at IS NULL`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.ErrNotFound
		}
		return classify("select dashboard", err)
	})
	return d, err
}

// ListDashboards는 삭제되지 않은 dashboard를 제목순(대소문자 무시, 동률은 id)으로 돌려준다. spec은 읽지 않는다(목록은 요약만).
func (s *DashboardStore) ListDashboards(ctx context.Context, p authz.Principal, after *DashboardPosition, limit int) ([]Dashboard, []DashboardPosition, bool, error) {
	if err := authz.Authorize(p, authz.DashboardsRead); err != nil {
		return nil, nil, false, err
	}
	if limit < 1 || limit > MaxDashboardPage {
		return nil, nil, false, fmt.Errorf("controldb: dashboard page limit must be within [1, %d]", MaxDashboardPage)
	}
	sql := `SELECT id::text, title, widget_count, revision, created_by, updated_by, created_at, updated_at, title_normalized
		FROM dashboards WHERE tenant_id = app_tenant_id() AND deleted_at IS NULL`
	args := []any{limit + 1}
	if after != nil {
		sql += ` AND (title_normalized, id) > ($2, $3::uuid)`
		args = append(args, after.TitleNormalized, after.ID)
	}
	sql += ` ORDER BY title_normalized, id LIMIT $1`
	var (
		out []Dashboard
		pos []DashboardPosition
	)
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return classify("list dashboards", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				d  Dashboard
				tn string
			)
			if err := rows.Scan(&d.ID, &d.Title, &d.WidgetCount, &d.Revision, &d.CreatedBy, &d.UpdatedBy, &d.CreatedAt, &d.UpdatedAt, &tn); err != nil {
				return classify("scan dashboard", err)
			}
			d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
			out = append(out, d)
			pos = append(pos, DashboardPosition{TitleNormalized: tn, ID: d.ID})
		}
		return classify("list dashboards", rows.Err())
	})
	if err != nil {
		return nil, nil, false, err
	}
	more := len(out) > limit
	if more {
		out, pos = out[:limit], pos[:limit]
	}
	return out, pos, more, nil
}

// UpdateDashboard는 ifMatch revision이 현재와 같을 때만 정의를 바꾼다(마지막 쓰기가 덮어쓰지 않는다, D02 §11).
// 없거나 삭제됐으면 authz.ErrNotFound, revision이 다르면 ErrRevisionMismatch다.
func (s *DashboardStore) UpdateDashboard(ctx context.Context, p authz.Principal, id string, ifMatch int64, w DashboardWrite, requestID string) (Dashboard, error) {
	if err := authz.Authorize(p, authz.DashboardsWrite); err != nil {
		return Dashboard{}, err
	}
	var d Dashboard
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var err error
		d, err = scanDashboard(tx.QueryRow(ctx, `
			UPDATE dashboards SET title = $3, spec = $4, widget_count = $5, revision = revision + 1, updated_by = $6, updated_at = now()
			WHERE id = $1 AND revision = $2 AND deleted_at IS NULL
			RETURNING `+dashboardColumns, id, ifMatch, w.Title, w.Spec, w.WidgetCount, p.Subject()))
		if errors.Is(err, pgx.ErrNoRows) {
			return dashboardMissOrMismatch(ctx, tx, id)
		}
		if err != nil {
			return classify("update dashboard", err)
		}
		details, err := dashboardDetails(d.Revision, w, false)
		if err != nil {
			return err
		}
		return writeAuditAndOutbox(ctx, tx, p.Tenant(), change{
			category: "operations", action: "dashboard.updated", actorKind: p.Kind().String(), actorID: p.Subject(),
			resourceType: "dashboard", resourceID: d.ID, revision: d.Revision, requestID: requestID, details: details,
		})
	})
	return d, err
}

// DeleteDashboard는 tombstone을 남긴다(revision + 1). ifMatch가 있으면 현재 revision과 같아야 한다. 반환값은 삭제 revision이다.
func (s *DashboardStore) DeleteDashboard(ctx context.Context, p authz.Principal, id string, ifMatch *int64, requestID string) (int64, error) {
	if err := authz.Authorize(p, authz.DashboardsWrite); err != nil {
		return 0, err
	}
	var revision int64
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var w DashboardWrite
		err := tx.QueryRow(ctx, `
			UPDATE dashboards SET deleted_at = now(), revision = revision + 1, updated_by = $3, updated_at = now()
			WHERE id = $1 AND ($2::bigint IS NULL OR revision = $2) AND deleted_at IS NULL
			RETURNING revision, title, spec, widget_count`, id, ifMatch, p.Subject()).Scan(&revision, &w.Title, &w.Spec, &w.WidgetCount)
		if errors.Is(err, pgx.ErrNoRows) {
			return dashboardMissOrMismatch(ctx, tx, id)
		}
		if err != nil {
			return classify("delete dashboard", err)
		}
		details, err := dashboardDetails(revision, w, true)
		if err != nil {
			return err
		}
		return writeAuditAndOutbox(ctx, tx, p.Tenant(), change{
			category: "operations", action: "dashboard.deleted", actorKind: p.Kind().String(), actorID: p.Subject(),
			resourceType: "dashboard", resourceID: id, revision: revision, requestID: requestID, details: details,
		})
	})
	return revision, err
}

func dashboardMissOrMismatch(ctx context.Context, tx pgx.Tx, id string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dashboards WHERE id = $1 AND deleted_at IS NULL)`, id).Scan(&exists); err != nil {
		return classify("select dashboard", err)
	}
	if exists {
		return ErrRevisionMismatch
	}
	return authz.ErrNotFound
}
