package controldb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

// 배포 이벤트 (D02 §14, D03 §12, ADR 0076).
const (
	// DeploymentRetention은 보존 기간이다(metric 1시간 rollup과 같다, 표시선을 그릴 수 있는 가장 긴 범위).
	DeploymentRetention = 395 * 24 * time.Hour
	// MaxDeploymentPage는 조회 page 하나의 최대 행 수다.
	MaxDeploymentPage = 500
	// DeploymentsPerMinute는 tenant마다 분당 새 행 상한이다(시작값, ADR 0076 §2).
	DeploymentsPerMinute = 60
	// deploymentExpireBatch는 쓰기 트랜잭션 하나가 지우는 만료 행 상한이다.
	deploymentExpireBatch = 1000
)

// ErrDeploymentRateLimited는 tenant의 분당 새 행 상한을 넘었다는 뜻이다(429).
var ErrDeploymentRateLimited = errors.New("controldb: deployment rate limit exceeded")

// Deployment는 배포 이벤트 한 건이다. 기록한 사람은 없다(감사에만, ADR 0076 §1).
type Deployment struct {
	ID               string
	ServiceNamespace string
	ServiceName      string
	Environment      string
	Version          string
	Commit           string // 없으면 ""
	OccurredAt       time.Time
	RecordedAt       time.Time
	Source           string
}

// DeploymentWrite는 검증을 마친 쓰기 입력이다.
type DeploymentWrite struct {
	ServiceNamespace string
	ServiceName      string
	Environment      string
	Version          string
	Commit           string
	OccurredAt       time.Time
}

// DeploymentResult는 쓰기 결과다.
type DeploymentResult struct {
	Stored StoredResponse
	// Replayed면 같은 Idempotency-Key의 첫 응답이다. Created면 새 행, 둘 다 아니면 같은 값의 기존 행이다.
	Replayed bool
	Created  bool
	// Expired는 이 트랜잭션에서 지운 만료 행 수다.
	Expired int64
}

// DeploymentStore는 배포 이벤트 저장소다.
type DeploymentStore struct{ db *DB }

// NewDeploymentStore는 저장소를 만든다.
func NewDeploymentStore(db *DB) *DeploymentStore { return &DeploymentStore{db: db} }

const deploymentColumns = `id::text, service_namespace, service_name, environment, version, commit_sha, occurred_at, recorded_at, source`

func scanDeployment(row pgx.Row) (Deployment, error) {
	var d Deployment
	if err := row.Scan(&d.ID, &d.ServiceNamespace, &d.ServiceName, &d.Environment, &d.Version, &d.Commit, &d.OccurredAt, &d.RecordedAt, &d.Source); err != nil {
		return Deployment{}, err
	}
	d.OccurredAt, d.RecordedAt = d.OccurredAt.UTC(), d.RecordedAt.UTC()
	return d, nil
}

// RecordDeployment는 배포 이벤트를 쓴다. 권한은 deployments.write이고 environment 제한 principal은 자기 environment만 쓴다.
//   - 같은 Idempotency-Key는 첫 응답이다(다른 본문이면 ErrIdempotencyConflict).
//   - key가 달라도 같은 값(서비스 키·환경·version·commit·시각)이면 기존 행이다. 바뀐 것이 없어 감사·outbox도 없다.
//   - 새 행만 분당 상한에 센다. 넘으면 ErrDeploymentRateLimited(트랜잭션을 되돌려 key도 남기지 않는다 — 나중에 다시 보낼 수 있다).
//   - 새 행은 감사·outbox와 같은 트랜잭션이다(D02 §03). 그 tenant의 만료 행을 함께 지운다(ADR 0076 §1).
func (s *DeploymentStore) RecordDeployment(ctx context.Context, p authz.Principal, w DeploymentWrite, idem IdempotencyRequest, requestID string, now time.Time,
	respond func(d Deployment, created bool) (StoredResponse, error)) (DeploymentResult, error) {
	if err := authz.Authorize(p, authz.DeploymentsWrite); err != nil {
		return DeploymentResult{}, err
	}
	if !p.AllowsEnvironment(w.Environment) {
		return DeploymentResult{}, authz.ErrForbidden
	}
	var res DeploymentResult
	stored, replayed, err := runIdempotent(ctx, s.db, p, idem, "record deployment", func(tx pgx.Tx) (StoredResponse, error) {
		// 재시도(unique 경쟁)로 다시 불릴 수 있다 — 이전 시도의 값을 남기지 않는다
		res = DeploymentResult{}
		existing := func() (Deployment, error) {
			return scanDeployment(tx.QueryRow(ctx, `
				SELECT `+deploymentColumns+` FROM deployments
				WHERE tenant_id = app_tenant_id() AND service_namespace = $1 AND service_name_normalized = lower($2)
				  AND environment = $3 AND version = $4 AND commit_sha = $5 AND occurred_at = $6`,
				w.ServiceNamespace, w.ServiceName, w.Environment, w.Version, w.Commit, w.OccurredAt))
		}
		d, err := existing()
		if err == nil {
			return respond(d, false)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return StoredResponse{}, classify("select deployment", err)
		}
		var recent int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM deployments WHERE tenant_id = app_tenant_id() AND recorded_at > $1`,
			now.Add(-time.Minute)).Scan(&recent); err != nil {
			return StoredResponse{}, classify("count deployments", err)
		}
		if recent >= DeploymentsPerMinute {
			return StoredResponse{}, ErrDeploymentRateLimited
		}
		id, err := newUUID()
		if err != nil {
			return StoredResponse{}, err
		}
		d, err = scanDeployment(tx.QueryRow(ctx, `
			INSERT INTO deployments (tenant_id, id, service_namespace, service_name, environment, version, commit_sha, occurred_at, recorded_at, expires_at)
			VALUES (app_tenant_id(), $1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (tenant_id, service_namespace, service_name_normalized, environment, version, commit_sha, occurred_at) DO NOTHING
			RETURNING `+deploymentColumns,
			id, w.ServiceNamespace, w.ServiceName, w.Environment, w.Version, w.Commit, w.OccurredAt, now, w.OccurredAt.Add(DeploymentRetention)))
		if errors.Is(err, pgx.ErrNoRows) { // 동시에 같은 값을 넣었다
			if d, err = existing(); err != nil {
				return StoredResponse{}, classify("select deployment", err)
			}
			return respond(d, false)
		}
		if err != nil {
			return StoredResponse{}, classify("insert deployment", err)
		}
		details, err := deploymentDetails(d)
		if err != nil {
			return StoredResponse{}, err
		}
		if err := writeAuditAndOutbox(ctx, tx, p.Tenant(), change{
			category: "operations", action: "deployment.recorded", actorKind: p.Kind().String(), actorID: p.Subject(),
			resourceType: "deployment", resourceID: d.ID, revision: 1, requestID: requestID, details: details,
		}); err != nil {
			return StoredResponse{}, err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM deployments WHERE (tenant_id, id) IN (
			SELECT tenant_id, id FROM deployments WHERE tenant_id = app_tenant_id() AND expires_at <= $1 LIMIT $2)`, now, deploymentExpireBatch)
		if err != nil {
			return StoredResponse{}, classify("delete expired deployments", err)
		}
		res.Created, res.Expired = true, tag.RowsAffected()
		return respond(d, true)
	})
	if err != nil {
		return DeploymentResult{}, err
	}
	res.Stored, res.Replayed = stored, replayed
	if replayed {
		res.Created, res.Expired = false, 0
	}
	return res, nil
}

// deploymentDetails는 감사·outbox 내용이다. 서비스 키·환경·version·commit만이다(자유 텍스트 없음).
func deploymentDetails(d Deployment) ([]byte, error) {
	b, err := json.Marshal(map[string]any{
		"service_namespace": d.ServiceNamespace, "service_name": d.ServiceName, "environment": d.Environment,
		"version": d.Version, "commit": d.Commit, "occurred_at": d.OccurredAt.Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, fmt.Errorf("controldb: deployment audit details: %w", err)
	}
	return b, nil
}

// DeploymentPosition은 keyset 위치다. (occurred_at, id) 내림차순에서 이보다 뒤부터 읽는다.
type DeploymentPosition struct {
	OccurredAt time.Time
	ID         string
}

// DeploymentQuery는 조회 조건이다.
type DeploymentQuery struct {
	ServiceName string
	// ServiceNamespace가 nil이면 모든 namespace, ""이면 namespace 없는 서비스만이다.
	ServiceNamespace *string
	// Environment가 nil이면 볼 수 있는 environment 전부다.
	Environment *string
	From, To    time.Time
	// RecordedBefore는 첫 page 시각이다. 그 뒤에 기록된 행은 이 page 묶음에서 읽지 않는다.
	RecordedBefore time.Time
	After          *DeploymentPosition
	Limit          int
}

// ListDeployments는 배포 이벤트를 시각 내림차순(동률은 id)으로 읽는다. 권한은 telemetry.read다.
// environment 제한 principal이 허용 밖 environment를 명시하면 ErrForbidden이다("없음"과 "못 봄"을 섞지 않는다).
// mandatory predicate: tenant(RLS·app_tenant_id)·시간·expires_at·environment 범위(계약 4). 삭제 tombstone은 F09와 함께 넣는다.
func (s *DeploymentStore) ListDeployments(ctx context.Context, p authz.Principal, q DeploymentQuery, now time.Time) ([]Deployment, bool, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, false, err
	}
	if q.Limit < 1 || q.Limit > MaxDeploymentPage {
		return nil, false, fmt.Errorf("controldb: deployment page limit must be within [1, %d]", MaxDeploymentPage)
	}
	if q.Environment != nil && !p.AllowsEnvironment(*q.Environment) {
		return nil, false, authz.ErrForbidden
	}
	sql := `SELECT ` + deploymentColumns + ` FROM deployments
		WHERE tenant_id = app_tenant_id() AND service_name_normalized = lower($1)
		  AND occurred_at >= $2 AND occurred_at < $3 AND expires_at > $4 AND recorded_at <= $5`
	args := []any{q.ServiceName, q.From, q.To, now, q.RecordedBefore}
	add := func(cond string, v any) {
		args = append(args, v)
		sql += fmt.Sprintf(" AND "+cond, len(args))
	}
	if q.ServiceNamespace != nil {
		add("service_namespace = $%d", *q.ServiceNamespace)
	}
	if q.Environment != nil {
		add("environment = $%d", *q.Environment)
	} else if p.EnvironmentRestricted() {
		add("environment = ANY($%d)", p.Environments())
	}
	if q.After != nil {
		args = append(args, q.After.OccurredAt, q.After.ID)
		sql += fmt.Sprintf(" AND (occurred_at, id) < ($%d, $%d::uuid)", len(args)-1, len(args))
	}
	args = append(args, q.Limit+1)
	sql += fmt.Sprintf(" ORDER BY occurred_at DESC, id DESC LIMIT $%d", len(args))
	var out []Deployment
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return classify("select deployments", err)
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDeployment(rows)
			if err != nil {
				return classify("scan deployment", err)
			}
			out = append(out, d)
		}
		return classify("select deployments", rows.Err())
	})
	if err != nil {
		return nil, false, err
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}
