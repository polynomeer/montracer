// Package servicemap은 span 쌍에서 서비스 간 호출 edge를 1분 window로 집계해 service_edges_1m에 쓰는 job이다
// (D02 §08 Map 산출 규칙, §10 service_edges_1m, ADR 0062).
//
// edge 하나는 (tenant, window, source 서비스, destination, connection)이다.
//
//   - client·producer span이 기준이다. 같은 trace에서 그 span을 부모로 둔 server·consumer span이 있고
//     서비스가 다르면 source → 그 서비스(observed). 요청 수·오류·latency는 client 쪽에서 센다.
//   - 짝이 없는 client span은 계측되지 않은 상대다: db.system → database, messaging.system → queue,
//     peer.service·내부 host 이름 → peer(조회 때 catalog 이름과 맞으면 estimated edge), 그 밖의 host → external.
//     host·IP 전체를 저장하지 않는다(외부 host는 등록 domain, IP는 한 node로 묶는다).
//   - client span이 없는 호출(부모가 다른 서비스의 internal·server span)은 server span으로 센다(fallback).
//     client 쪽에서 이미 센 호출은 다시 세지 않는다(이중 계수 금지).
//   - messaging은 처리 시간(consumer span)과 큐 대기(consumer 시작 − producer 끝)를 나눠 담는다.
//
// span은 표본일 수 있으므로 edge의 수는 표본 수다(조회 meta.sampled=true). SDK dependency metric이 생기면 그쪽을 쓴다.
package servicemap

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

// Bounds는 latency·큐 대기 histogram의 경계(ms)다. OTel SDK 기본 explicit bucket과 같다.
// 바꾸면 조회는 경계가 다른 window를 합치지 않는다(경고).
var Bounds = []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000}

// span kind (OTLP SpanKind)
const (
	KindServer   uint8 = 2
	KindClient   uint8 = 3
	KindProducer uint8 = 4
	KindConsumer uint8 = 5
)

// Destination 종류 (service_edges_1m.destination_kind)
const (
	DestService  = "service"
	DestDatabase = "database"
	DestQueue    = "queue"
	DestExternal = "external"
	DestPeer     = "peer" // 이름만 안다: 조회 때 catalog와 맞으면 estimated service edge, 아니면 external
)

// Connection 종류
const (
	ConnRPC       = "rpc"
	ConnMessaging = "messaging"
)

// maxKeyLen은 destination key 상한이다(긴 속성 값이 그대로 key가 되지 않게).
const maxKeyLen = 200

// Pair는 저장소가 집계해 돌려주는 (window, source, 상대) 묶음이다.
// LatencyLE·WaitLE는 경계 이하 관측 수(누적)다. 길이는 len(Bounds)다.
type Pair struct {
	Tenant      string
	WindowStart time.Time
	Source      string // service_id
	Matched     bool   // 상대 span(server·consumer, fallback이면 부모)을 찾았다
	Target      string // Matched일 때 상대 service_id
	Kind        uint8  // 센 span의 kind
	// 짝이 없는 client span의 상대 속성
	Peer, DB, DBNamespace, Messaging, Host string

	Requests, Errors uint64
	LatencyLE        []uint64
	WaitCount        uint64
	WaitLE           []uint64
	LastSeen         time.Time
}

// Row는 service_edges_1m 한 행이다. Buckets·WaitBuckets는 bucket별 수(len(Bounds)+1, 마지막은 +Inf)다.
type Row struct {
	Tenant          string
	WindowStart     time.Time
	Source          string
	Destination     string // service_id 또는 "db:…", "queue:…", "peer:…", "host:…"
	DestinationKind string
	Connection      string
	Requests        uint64
	Errors          uint64
	Bounds          []float64
	Buckets         []uint64
	WaitCount       uint64
	WaitBuckets     []uint64
	LastSeen        time.Time
	Revision        uint64
	ComputedAt      time.Time
	ExpiresAt       time.Time
}

// Destination은 짝이 있으면 상대 서비스, 없으면 상대 속성으로 정한 destination이다.
// 알 수 없는 상대(속성 없음)와 자기 자신 호출은 ok=false다.
func Destination(p Pair) (key, kind string, ok bool) {
	if p.Matched {
		if p.Target == "" || p.Target == p.Source {
			return "", "", false
		}
		return p.Target, DestService, true
	}
	switch {
	case p.DB != "":
		k := "db:" + strings.ToLower(p.DB)
		if p.DBNamespace != "" {
			k += "/" + p.DBNamespace
		}
		return clip(k), DestDatabase, true
	case p.Messaging != "":
		return clip("queue:" + strings.ToLower(p.Messaging)), DestQueue, true
	case p.Peer != "":
		return clip("peer:" + p.Peer), DestPeer, true
	case p.Host != "":
		return hostDestination(p.Host)
	}
	return "", "", false
}

// hostDestination은 host 전체를 남기지 않는다(D02 §08).
//   - IP는 "host:ip" 한 node
//   - 점이 없는 이름과 cluster 내부 이름(*.svc, *.cluster.local)은 첫 label을 서비스 이름 후보로(peer)
//   - 그 밖은 등록 domain(eTLD+1)
func hostDestination(host string) (string, string, bool) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" {
		return "", "", false
	}
	if _, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		return "host:ip", DestExternal, true
	}
	if !strings.Contains(h, ".") || strings.HasSuffix(h, ".svc") || strings.HasSuffix(h, ".cluster.local") {
		first, _, _ := strings.Cut(h, ".")
		return clip("peer:" + first), DestPeer, true
	}
	if d, err := publicsuffix.EffectiveTLDPlusOne(h); err == nil {
		return clip("host:" + d), DestExternal, true
	}
	return clip("host:" + h), DestExternal, true
}

func clip(s string) string {
	if len(s) <= maxKeyLen {
		return s
	}
	// UTF-8 경계에서 자른다
	cut := maxKeyLen
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}

// ConnectionOf는 센 span의 kind로 정한 연결 종류다.
func ConnectionOf(kind uint8) string {
	if kind == KindProducer || kind == KindConsumer {
		return ConnMessaging
	}
	return ConnRPC
}

type rowKey struct {
	tenant      string
	window      int64
	source      string
	destination string
	connection  string
}

// Build는 pair를 edge 행으로 합친다(같은 key의 pair는 더한다). 버린 pair 수(상대 불명·자기 호출)를 함께 돌려준다.
// 결과는 (tenant, window, source, destination, connection) 순이다.
func Build(pairs []Pair) ([]Row, int) {
	rows := map[rowKey]*Row{}
	dropped := 0
	for _, p := range pairs {
		dest, kind, ok := Destination(p)
		if !ok {
			dropped++
			continue
		}
		conn := ConnectionOf(p.Kind)
		k := rowKey{p.Tenant, p.WindowStart.Unix(), p.Source, dest, conn}
		r, ok := rows[k]
		if !ok {
			r = &Row{Tenant: p.Tenant, WindowStart: p.WindowStart.UTC(), Source: p.Source, Destination: dest,
				DestinationKind: kind, Connection: conn, Bounds: Bounds,
				Buckets: make([]uint64, len(Bounds)+1), WaitBuckets: make([]uint64, len(Bounds)+1)}
			rows[k] = r
		}
		r.Requests += p.Requests
		r.Errors += p.Errors
		addCumulative(r.Buckets, p.LatencyLE, p.Requests)
		r.WaitCount += p.WaitCount
		addCumulative(r.WaitBuckets, p.WaitLE, p.WaitCount)
		if p.LastSeen.After(r.LastSeen) {
			r.LastSeen = p.LastSeen.UTC()
		}
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(a, b int) bool { return less(out[a], out[b]) })
	return out, dropped
}

func less(a, b Row) bool {
	switch {
	case a.Tenant != b.Tenant:
		return a.Tenant < b.Tenant
	case !a.WindowStart.Equal(b.WindowStart):
		return a.WindowStart.Before(b.WindowStart)
	case a.Source != b.Source:
		return a.Source < b.Source
	case a.Destination != b.Destination:
		return a.Destination < b.Destination
	default:
		return a.Connection < b.Connection
	}
}

// addCumulative는 경계 이하 누적 수(le)를 bucket별 수로 바꿔 더한다. total은 전체 관측 수(+Inf 포함)다.
// le 길이가 경계와 다르면(저장소 오류) 모두 +Inf로 넣는다 — 관측 수를 잃지 않는다.
func addCumulative(dst, le []uint64, total uint64) {
	n := len(dst) - 1
	if len(le) != n {
		dst[n] += total
		return
	}
	var prev uint64
	for i, c := range le {
		if c < prev { // 누적이 줄면 저장소 값이 깨진 것이다. 앞 값으로 둔다
			c = prev
		}
		dst[i] += c - prev
		prev = c
	}
	if total > prev {
		dst[n] += total - prev
	}
}

// windowHash는 tenant 하나의 window 내용(정렬된 행) 해시다. revision·계산 시각은 빼고 계산한다.
func windowHash(rows []Row) [32]byte {
	h := sha256.New()
	w := func(s string) { _, _ = h.Write([]byte(s)); _, _ = h.Write([]byte{0}) }
	u := func(v uint64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], v)
		_, _ = h.Write(b[:])
	}
	for _, r := range rows {
		w(r.Tenant)
		u(uint64(r.WindowStart.Unix())) //nolint:gosec // 1970 이후 시각
		w(r.Source)
		w(r.Destination)
		w(r.DestinationKind)
		w(r.Connection)
		u(r.Requests)
		u(r.Errors)
		for _, b := range r.Bounds {
			w(fmt.Sprint(b))
		}
		for _, b := range r.Buckets {
			u(b)
		}
		u(r.WaitCount)
		for _, b := range r.WaitBuckets {
			u(b)
		}
		u(uint64(r.LastSeen.UnixNano())) //nolint:gosec // 1970 이후 시각
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ContentToken은 rows 전체의 insert dedup token이다(재시도해도 같은 내용이면 한 번만 들어간다).
func ContentToken(rows []Row) string {
	h := sha256.New()
	c := windowHash(rows)
	_, _ = h.Write(c[:])
	for _, r := range rows {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], r.Revision)
		_, _ = h.Write(b[:])
	}
	return fmt.Sprintf("servicemap/%x", h.Sum(nil)[:16])
}
