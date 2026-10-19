package servicemap

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	svcA = "11111111-1111-4111-8111-111111111111"
	svcB = "22222222-2222-4222-8222-222222222222"
)

func TestDestination(t *testing.T) {
	cases := []struct {
		name    string
		p       Pair
		key     string
		kind    string
		dropped bool
	}{
		{"짝이 있는 서비스", Pair{Source: svcA, Matched: true, Target: svcB}, svcB, DestService, false},
		{"자기 호출은 버린다", Pair{Source: svcA, Matched: true, Target: svcA}, "", "", true},
		{"DB system과 namespace", Pair{Source: svcA, DB: "PostgreSQL", DBNamespace: "orders"}, "db:postgresql/orders", DestDatabase, false},
		{"DB가 peer.service보다 먼저", Pair{Source: svcA, DB: "redis", Peer: "cache"}, "db:redis", DestDatabase, false},
		{"queue는 system만", Pair{Source: svcA, Messaging: "Kafka"}, "queue:kafka", DestQueue, false},
		{"peer.service", Pair{Source: svcA, Peer: "payment"}, "peer:payment", DestPeer, false},
		{"IP는 한 node", Pair{Source: svcA, Host: "10.0.3.7"}, "host:ip", DestExternal, false},
		{"IPv6", Pair{Source: svcA, Host: "[2001:db8::1]"}, "host:ip", DestExternal, false},
		{"외부 host는 등록 domain", Pair{Source: svcA, Host: "api.eu.stripe.com"}, "host:stripe.com", DestExternal, false},
		{"공개 suffix 두 단계", Pair{Source: svcA, Host: "pay.example.co.uk"}, "host:example.co.uk", DestExternal, false},
		{"점 없는 이름은 서비스 후보", Pair{Source: svcA, Host: "payment"}, "peer:payment", DestPeer, false},
		{"cluster 내부 이름은 서비스 후보", Pair{Source: svcA, Host: "payment.shop.svc.cluster.local"}, "peer:payment", DestPeer, false},
		{"상대 속성이 없으면 버린다", Pair{Source: svcA}, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, kind, ok := Destination(c.p)
			if ok == c.dropped || key != c.key || kind != c.kind {
				t.Fatalf("Destination = (%q, %q, %v), want (%q, %q, %v)", key, kind, ok, c.key, c.kind, !c.dropped)
			}
		})
	}
}

func TestDestinationClipsLongKeys(t *testing.T) {
	key, _, ok := Destination(Pair{Source: svcA, Peer: strings.Repeat("가", 200)})
	if !ok || len(key) > maxKeyLen || !strings.HasPrefix(key, "peer:") {
		t.Fatalf("key length %d", len(key))
	}
	if !strings.HasSuffix(key, "가") {
		t.Fatal("clip cut inside a UTF-8 sequence")
	}
}

func TestBuildMergesAndConvertsBuckets(t *testing.T) {
	w := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	le := func(v ...uint64) []uint64 {
		out := make([]uint64, len(Bounds))
		copy(out, v)
		for i := len(v); i < len(out); i++ {
			out[i] = v[len(v)-1]
		}
		return out
	}
	pairs := []Pair{
		// client 쪽 2건(≤0ms 1, ≤5ms 2), 오류 1
		{Tenant: "t", WindowStart: w, Source: svcA, Matched: true, Target: svcB, Kind: KindClient, Requests: 2, Errors: 1,
			LatencyLE: le(1, 2), LastSeen: w.Add(10 * time.Second)},
		// fallback(server) 3건이 같은 edge에 더해진다. 2건은 10초 넘음(+Inf)
		{Tenant: "t", WindowStart: w, Source: svcA, Matched: true, Target: svcB, Kind: KindServer, Requests: 3,
			LatencyLE: le(0, 0, 1), LastSeen: w.Add(20 * time.Second)},
		// 같은 상대지만 messaging은 다른 edge
		{Tenant: "t", WindowStart: w, Source: svcA, Matched: true, Target: svcB, Kind: KindProducer, Requests: 1,
			LatencyLE: le(0, 1), WaitCount: 1, WaitLE: le(0, 0, 0, 1), LastSeen: w},
		// 버려지는 pair
		{Tenant: "t", WindowStart: w, Source: svcA, Requests: 4},
	}
	rows, dropped := Build(pairs)
	if dropped != 1 || len(rows) != 2 {
		t.Fatalf("rows = %d, dropped = %d", len(rows), dropped)
	}
	msg, rpc := rows[0], rows[1] // connection 순 (messaging < rpc)
	if rpc.Connection != ConnRPC || rpc.Requests != 5 || rpc.Errors != 1 || !rpc.LastSeen.Equal(w.Add(20*time.Second)) {
		t.Fatalf("rpc edge = %+v", rpc)
	}
	want := make([]uint64, len(Bounds)+1)
	want[0], want[1], want[2], want[len(Bounds)] = 1, 1, 1, 2
	if !slices.Equal(rpc.Buckets, want) {
		t.Fatalf("rpc buckets = %v, want %v", rpc.Buckets, want)
	}
	if msg.WaitCount != 1 || msg.WaitBuckets[3] != 1 || msg.Buckets[1] != 1 {
		t.Fatalf("messaging edge = %+v", msg)
	}
}

func TestAddCumulativeKeepsCountsOnBadInput(t *testing.T) {
	dst := make([]uint64, 3)
	addCumulative(dst, []uint64{1}, 4) // 길이가 다르면 모두 +Inf
	if dst[2] != 4 {
		t.Fatalf("dst = %v", dst)
	}
	dst = make([]uint64, 3)
	addCumulative(dst, []uint64{3, 2}, 3) // 누적이 줄면 앞 값으로
	if dst[0] != 3 || dst[1] != 0 || dst[2] != 0 {
		t.Fatalf("dst = %v", dst)
	}
}

func TestWindowHashIgnoresRevision(t *testing.T) {
	w := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	rows, _ := Build([]Pair{{Tenant: "t", WindowStart: w, Source: svcA, Peer: "x", Kind: KindClient, Requests: 1, LatencyLE: make([]uint64, len(Bounds))}})
	a := windowHash(rows)
	rows[0].Revision, rows[0].ComputedAt = 9, w
	if windowHash(rows) != a {
		t.Fatal("hash depends on revision")
	}
	rows[0].Requests = 2
	if windowHash(rows) == a {
		t.Fatal("hash ignores content")
	}
}
