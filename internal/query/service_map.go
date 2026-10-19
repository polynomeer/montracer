package query

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/metricagg"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// ServiceMapStore는 service map edge 저장소다 (telemetrystore.QueryStore, ADR 0062).
type ServiceMapStore interface {
	ServiceEdges(ctx context.Context, p authz.Principal, q telemetrystore.ServiceEdgeQuery, now time.Time) ([]telemetrystore.ServiceEdge, bool, error)
	ServiceEdgeWatermark(ctx context.Context, p authz.Principal, since, now time.Time) (time.Time, error)
}

const (
	// MaxMapNodes·MaxMapEdges는 service map 결과 기본 상한이다(D02 §13 "500 nodes·2,000 edges"). 넘으면 omitted_count로 밝힌다.
	MaxMapNodes = 500
	MaxMapEdges = 2000
	// maxMapServiceIDs는 요청 service_ids 상한이다.
	maxMapServiceIDs = 100
)

// map 경고 (meta.warnings)
const (
	// warnCrossEnvironment는 environment가 다른 서비스 사이 edge를 숨겼다는 뜻이다(D02 §08 "tenant와 environment 내에서만 연결").
	warnCrossEnvironment = "cross_environment_edges_hidden"
	// warnEdgesTruncated는 저장소 읽기 상한에 걸려 요청 수가 적은 edge를 읽지 못했다는 뜻이다.
	warnEdgesTruncated = "edge_rows_truncated"
	// warnBoundsMismatch는 histogram 경계가 다른 window를 합치지 못해 latency를 비웠다는 뜻이다.
	warnBoundsMismatch = "latency_bounds_mismatch"
)

// serviceMapRequest는 POST /api/v1/service-map/query 본문이다 (D02 §13 "range, environment, service_ids").
type serviceMapRequest struct {
	Range       requestRange `json:"range"`
	Environment string       `json:"environment"`
	ServiceIDs  []string     `json:"service_ids"`
}

// MapNode는 map node다. service는 catalog 서비스, 나머지는 계측되지 않은 상대(source 서비스의 environment 안에서만 묶는다).
type MapNode struct {
	NodeID      string  `json:"node_id"`
	Kind        string  `json:"kind"` // service | database | queue | external
	Name        string  `json:"name"`
	ServiceID   *string `json:"service_id"`
	Namespace   *string `json:"namespace"`
	Environment string  `json:"environment"`
}

// MapLatency는 bucket을 병합해 계산한 분위수(ms)다(계약 5). 관측이 없으면 null이다.
type MapLatency struct {
	P50Ms *float64 `json:"p50_ms"`
	P95Ms *float64 `json:"p95_ms"`
	P99Ms *float64 `json:"p99_ms"`
}

// MapEdge는 source → destination 호출이다. estimated(이름으로만 추정한 서비스)는 수와 latency가 null이다(D05 §07).
type MapEdge struct {
	Source      string      `json:"source"`
	Destination string      `json:"destination"`
	Connection  string      `json:"connection"` // rpc | messaging
	Confidence  string      `json:"confidence"` // observed | estimated
	Requests    *uint64     `json:"requests"`
	Errors      *uint64     `json:"errors"`
	Latency     *MapLatency `json:"latency"`
	QueueWait   *MapLatency `json:"queue_wait"` // messaging만
	LastSeen    string      `json:"last_seen"`
}

type mapOmitted struct {
	Nodes int `json:"nodes"`
	Edges int `json:"edges"`
}

type serviceMapResponse struct {
	Data struct {
		Nodes        []MapNode  `json:"nodes"`
		Edges        []MapEdge  `json:"edges"`
		OmittedCount mapOmitted `json:"omitted_count"`
		Range        TimeRange  `json:"range"`
	} `json:"data"`
	Meta Meta `json:"meta"`
}

// TimeRange는 응답의 [from, to)다(RFC3339 UTC).
type TimeRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (r serviceMapRequest) validate() error {
	if r.Range.From.IsZero() || r.Range.To.IsZero() || !r.Range.From.Before(r.Range.To) {
		return invalid("range", "from and to are required and from must be before to (RFC3339 UTC)")
	}
	if r.Range.To.Sub(r.Range.From) > telemetrystore.MaxServiceMapRange {
		e := apierr.New(apierr.QueryBudgetExceeded, "조회 범위를 줄이세요")
		e.Details = map[string]any{"max_range_seconds": int64(telemetrystore.MaxServiceMapRange / time.Second)}
		return e
	}
	if len(r.Environment) > 255 {
		return invalid("environment", "at most 255 characters")
	}
	if len(r.ServiceIDs) > maxMapServiceIDs {
		return invalid("service_ids", "at most 100 ids")
	}
	for _, id := range r.ServiceIDs {
		if !serviceIDPattern.MatchString(id) {
			return invalid("service_ids", "must be lowercase UUIDs")
		}
	}
	return nil
}

// queryServiceMap은 POST /api/v1/service-map/query다 (D02 §08, §13, ADR 0062).
//
// edge는 span 쌍의 1분 집계(service_edges_1m)를 범위만큼 합친 것이다. span은 표본일 수 있어 meta.sampled=true다.
// 볼 수 없는 서비스(다른 environment 제한·catalog에 없음)에 닿는 edge는 통째로 뺀다 — 이름·수를 남기지 않는다(D02 §08).
func (h *Handler) queryServiceMap(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.ServiceMap == nil || h.cfg.Services == nil {
		return apierr.New(apierr.NotFound, "service map을 사용할 수 없습니다")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		e := apierr.New(apierr.InvalidArgument, "요청 본문이 너무 큽니다")
		e.Status = http.StatusRequestEntityTooLarge
		return e
	}
	var req serviceMapRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return invalid("body", "must be a JSON service map query")
	}
	if err := req.validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.QueryTimeout)
	defer cancel()
	// 입력 검증을 마친 뒤에만 tenant 실행 slot을 잡는다 (D02 §15)
	release, err := h.gate.acquire(ctx, p.Tenant().String())
	if err != nil {
		return err
	}
	defer release()
	now := h.cfg.Now()

	q := telemetrystore.ServiceEdgeQuery{Range: telemetrystore.TimeRange{From: req.Range.From.UTC(), To: req.Range.To.UTC()}}
	if p.EnvironmentRestricted() {
		if q.EnvironmentServices, err = h.cfg.Services.EnvironmentServiceIDs(ctx, p); err != nil {
			return err
		}
	}
	edges, truncated, err := h.cfg.ServiceMap.ServiceEdges(ctx, p, q, now)
	if err != nil {
		return err
	}
	m, err := h.resolveMap(ctx, p, edges, req)
	if err != nil {
		return err
	}
	wm, err := h.cfg.ServiceMap.ServiceEdgeWatermark(ctx, p, q.Range.From.Truncate(time.Minute), now)
	if err != nil {
		return err
	}

	resp := serviceMapResponse{Meta: newMeta(r)}
	resp.Data.Nodes, resp.Data.Edges, resp.Data.OmittedCount = limitMap(m.nodes, m.edges)
	resp.Data.Range = TimeRange{From: q.Range.From.Truncate(time.Minute).Format(time.RFC3339), To: q.Range.To.Format(time.RFC3339Nano)}
	sampled, resolution := true, 60
	resp.Meta.Sampled, resp.Meta.ResolutionSeconds = &sampled, &resolution
	if !wm.IsZero() {
		s := wm.Format(time.RFC3339)
		resp.Meta.Watermark = &s
	}
	if m.crossEnv {
		resp.Meta.Warnings = append(resp.Meta.Warnings, warnCrossEnvironment)
	}
	if truncated {
		resp.Meta.Warnings = append(resp.Meta.Warnings, warnEdgesTruncated)
	}
	if m.boundsMismatch {
		resp.Meta.Warnings = append(resp.Meta.Warnings, warnBoundsMismatch)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	return json.NewEncoder(w).Encode(resp)
}

// resolvedMap은 catalog로 이름을 붙이고 범위로 거른 map이다(상한 적용 전).
type resolvedMap struct {
	nodes          map[string]MapNode
	edges          []mapEdgeAcc
	crossEnv       bool
	boundsMismatch bool
}

// mapEdgeAcc는 합치는 중인 edge다. estimated는 수를 공개하지 않는다.
type mapEdgeAcc struct {
	MapEdge
	requests, errors uint64
	bounds           []float64
	buckets, wait    []uint64
	waitCount        uint64
	lastSeen         time.Time
	mismatch         bool
}

// resolveMap은 저장소 edge에 catalog를 붙인다.
//   - source·destination 서비스는 principal이 볼 수 있어야 한다(ServicesByID가 범위를 적용한다). 못 보면 edge를 뺀다.
//   - environment가 다른 두 서비스 사이 edge는 숨기고 경고한다.
//   - peer(이름만 아는 상대)는 source와 같은 environment에서 이름이 같은 서비스가 하나 있으면 estimated edge, 아니면 external node다.
//   - 요청 environment·service_ids로 좁힌다.
func (h *Handler) resolveMap(ctx context.Context, p authz.Principal, edges []telemetrystore.ServiceEdge, req serviceMapRequest) (resolvedMap, error) {
	ids := map[string]bool{}
	peers := map[string]bool{}
	for _, e := range edges {
		ids[e.Source] = true
		switch e.DestinationKind {
		case "service":
			ids[e.Destination] = true
		case "peer":
			peers[strings.TrimPrefix(e.Destination, "peer:")] = true
		}
	}
	byName, err := h.resolvePeerNames(ctx, p, peers)
	if err != nil {
		return resolvedMap{}, err
	}
	for _, list := range byName {
		for _, id := range list {
			ids[id] = true
		}
	}
	services, err := h.servicesByID(ctx, p, ids)
	if err != nil {
		return resolvedMap{}, err
	}

	want := map[string]bool{}
	for _, id := range req.ServiceIDs {
		want[id] = true
	}
	out := resolvedMap{nodes: map[string]MapNode{}}
	acc := map[[3]string]*mapEdgeAcc{} // (source, destination node, connection) + confidence는 destination으로 갈린다
	for _, e := range edges {
		src, ok := services[e.Source]
		if !ok {
			continue // 볼 수 없거나 catalog에 없다(fail closed)
		}
		if req.Environment != "" && src.Environment != req.Environment {
			continue
		}
		var dst MapNode
		confidence := "observed"
		switch e.DestinationKind {
		case "service":
			d, ok := services[e.Destination]
			if !ok {
				continue
			}
			if d.Environment != src.Environment {
				out.crossEnv = true
				continue
			}
			dst = serviceNode(d)
		case "peer":
			name := strings.TrimPrefix(e.Destination, "peer:")
			if d, ok := samePeer(byName[name], services, src.Environment); ok {
				if d.ServiceID == src.ServiceID {
					continue // 자기 자신을 이름으로 부른 호출
				}
				dst, confidence = serviceNode(d), "estimated"
			} else {
				dst = externalNode("external", "peer:"+name, name, src.Environment)
			}
		case "database":
			dst = externalNode("database", e.Destination, strings.TrimPrefix(e.Destination, "db:"), src.Environment)
		case "queue":
			dst = externalNode("queue", e.Destination, strings.TrimPrefix(e.Destination, "queue:"), src.Environment)
		default:
			dst = externalNode("external", e.Destination, strings.TrimPrefix(e.Destination, "host:"), src.Environment)
		}
		if len(want) > 0 && !want[src.ServiceID] && (dst.ServiceID == nil || !want[*dst.ServiceID]) {
			continue
		}
		out.nodes[src.ServiceID] = serviceNode(src)
		out.nodes[dst.NodeID] = dst
		k := [3]string{src.ServiceID, dst.NodeID + "|" + confidence, e.Connection}
		a, ok := acc[k]
		if !ok {
			a = &mapEdgeAcc{MapEdge: MapEdge{Source: src.ServiceID, Destination: dst.NodeID, Connection: e.Connection, Confidence: confidence},
				bounds: e.Bounds, buckets: make([]uint64, len(e.Buckets)), wait: make([]uint64, len(e.WaitBuckets))}
			acc[k] = a
		}
		a.add(e)
	}
	for _, a := range acc {
		out.boundsMismatch = out.boundsMismatch || a.mismatch
		out.edges = append(out.edges, *a)
	}
	return out, nil
}

func (a *mapEdgeAcc) add(e telemetrystore.ServiceEdge) {
	a.requests += e.Requests
	a.errors += e.Errors
	a.waitCount += e.WaitCount
	if e.LastSeen.After(a.lastSeen) {
		a.lastSeen = e.LastSeen
	}
	if !slices.Equal(a.bounds, e.Bounds) || len(a.buckets) != len(e.Buckets) || len(a.wait) != len(e.WaitBuckets) {
		a.mismatch = true // 경계가 다른 window는 합치지 않는다(분위수를 만들지 않음)
		return
	}
	for i, b := range e.Buckets {
		a.buckets[i] += b
	}
	for i, b := range e.WaitBuckets {
		a.wait[i] += b
	}
}

// edge는 응답 edge다. estimated는 정확한 수·latency를 붙이지 않는다(D05 §07).
func (a *mapEdgeAcc) edge() MapEdge {
	e := a.MapEdge
	e.LastSeen = a.lastSeen.Format(time.RFC3339Nano)
	if e.Confidence == "estimated" {
		return e
	}
	req, errs := a.requests, a.errors
	e.Requests, e.Errors = &req, &errs
	if !a.mismatch {
		e.Latency = quantiles(a.bounds, a.buckets)
		if e.Connection == "messaging" && a.waitCount > 0 {
			e.QueueWait = quantiles(a.bounds, a.wait)
		}
	}
	return e
}

func quantiles(bounds []float64, buckets []uint64) *MapLatency {
	q := func(v float64) *float64 {
		x, ok := metricagg.Quantile(v, bounds, buckets)
		if !ok {
			return nil
		}
		return &x
	}
	l := MapLatency{P50Ms: q(0.5), P95Ms: q(0.95), P99Ms: q(0.99)}
	if l.P50Ms == nil {
		return nil // 관측 없음
	}
	return &l
}

func serviceNode(s controldb.Service) MapNode {
	id, ns := s.ServiceID, s.Namespace
	return MapNode{NodeID: id, Kind: "service", Name: s.Name, ServiceID: &id, Namespace: &ns, Environment: s.Environment}
}

// externalNode는 계측되지 않은 상대다. 같은 상대라도 environment마다 다른 node다(environment를 넘어 잇지 않는다).
func externalNode(kind, key, name, env string) MapNode {
	return MapNode{NodeID: key + "@" + env, Kind: kind, Name: name, Environment: env}
}

// samePeer는 source와 같은 environment에서 이름이 같은 서비스다. 둘 이상이면(namespace만 다름) 고르지 않는다.
func samePeer(ids []string, services map[string]controldb.Service, env string) (controldb.Service, bool) {
	var found []controldb.Service
	for _, id := range ids {
		if s, ok := services[id]; ok && s.Environment == env {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		return controldb.Service{}, false
	}
	return found[0], true
}

// resolvePeerNames는 peer 이름 → 볼 수 있는 service_id 목록이다(대소문자 무시). 이름 상한씩 나눠 묻는다.
func (h *Handler) resolvePeerNames(ctx context.Context, p authz.Principal, peers map[string]bool) (map[string][]string, error) {
	names := make([]string, 0, len(peers))
	for n := range peers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := map[string][]string{}
	for chunk := range slices.Chunk(names, controldb.MaxResolveNames) {
		res, err := h.cfg.Services.ResolveServiceNames(ctx, p, chunk)
		if err != nil {
			return nil, err
		}
		for k, v := range res {
			out[k] = v
		}
	}
	return out, nil
}

// servicesByID는 catalog 항목을 id 상한씩 나눠 읽는다.
func (h *Handler) servicesByID(ctx context.Context, p authz.Principal, ids map[string]bool) (map[string]controldb.Service, error) {
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	sort.Strings(list)
	out := map[string]controldb.Service{}
	for chunk := range slices.Chunk(list, controldb.MaxServiceNameLookup) {
		res, err := h.cfg.Services.ServicesByID(ctx, p, chunk)
		if err != nil {
			return nil, err
		}
		for k, v := range res {
			out[k] = v
		}
	}
	return out, nil
}

// limitMap은 node·edge 상한을 적용한다. 요청 수가 많은 node·edge부터 남기고(estimated는 0으로 본다),
// 남긴 node 사이 edge만 남긴다. 남긴 edge가 닿지 않는 node는 뺀다. 뺀 수를 돌려준다.
func limitMap(nodes map[string]MapNode, edges []mapEdgeAcc) ([]MapNode, []MapEdge, mapOmitted) {
	traffic := map[string]uint64{}
	weight := func(a mapEdgeAcc) uint64 {
		if a.Confidence == "estimated" {
			return 0
		}
		return a.requests
	}
	for _, a := range edges {
		traffic[a.Source] += weight(a)
		traffic[a.Destination] += weight(a)
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if traffic[ids[i]] != traffic[ids[j]] {
			return traffic[ids[i]] > traffic[ids[j]]
		}
		return ids[i] < ids[j]
	})
	keep := map[string]bool{}
	for _, id := range ids[:min(len(ids), MaxMapNodes)] {
		keep[id] = true
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if weight(a) != weight(b) {
			return weight(a) > weight(b)
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Destination != b.Destination {
			return a.Destination < b.Destination
		}
		if a.Connection != b.Connection {
			return a.Connection < b.Connection
		}
		return a.Confidence < b.Confidence
	})
	outEdges := []MapEdge{}
	used := map[string]bool{}
	for _, a := range edges {
		if len(outEdges) == MaxMapEdges {
			break
		}
		if !keep[a.Source] || !keep[a.Destination] {
			continue
		}
		outEdges = append(outEdges, a.edge())
		used[a.Source], used[a.Destination] = true, true
	}
	outNodes := []MapNode{}
	for _, id := range ids {
		if used[id] {
			outNodes = append(outNodes, nodes[id])
		}
	}
	sort.Slice(outNodes, func(i, j int) bool { return outNodes[i].NodeID < outNodes[j].NodeID })
	return outNodes, outEdges, mapOmitted{Nodes: len(nodes) - len(outNodes), Edges: len(edges) - len(outEdges)}
}
