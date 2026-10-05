// Package pipeline은 수집 worker의 처리 단계다 (D02 §05, §09~10, §21~22, ADR 0020, 0021).
//
//	Kafka record → envelope header 검증 → 단일 record OTLP 해석·정규화 → (tenant, event_id) dedup
//	→ ClickHouse 동기 insert (partition offset 범위 token) → offset commit
//
// tenant는 mt-tenant-id header 하나에서만 온다. payload의 어떤 속성도 tenant로 해석하지 않는다.
// 해석할 수 없는 record는 원문 없이 위치·해시만 quarantine에 남긴다.
package pipeline

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetry/envelope"
)

// Message는 Kafka record 하나다. 소비 클라이언트와 무관한 형태로 둔다.
type Message struct {
	Topic     string
	Partition int32
	Offset    int64
	Key       []byte
	Value     []byte
	Headers   []envelope.Header
}

// Signal은 수집 신호다.
type Signal string

// 신호.
const (
	SignalTraces  Signal = "traces"
	SignalLogs    Signal = "logs"
	SignalMetrics Signal = "metrics"
)

var topicSignal = map[string]Signal{
	envelope.TopicTraces:  SignalTraces,
	envelope.TopicLogs:    SignalLogs,
	envelope.TopicMetrics: SignalMetrics,
}

// Quarantine 사유. 값 내용은 담지 않는다.
const (
	ReasonUnknownTopic      = "unknown_topic"
	ReasonMissingHeader     = "missing_header"
	ReasonInvalidHeader     = "invalid_header"
	ReasonUnknownSchema     = "unknown_schema_version"
	ReasonSignalMismatch    = "signal_mismatch"
	ReasonTenantKeyMismatch = "tenant_key_mismatch"
	ReasonDecode            = "decode_failed"
	ReasonShape             = "not_single_record"
	ReasonEventIDMismatch   = "event_id_mismatch"
)

// meta는 header에서 읽은 envelope 메타데이터다.
type meta struct {
	tenant        authz.TenantID
	signal        Signal
	eventID       string
	receivedAt    time.Time
	policyVersion int64
	routingEpoch  int64
}

// rejectError는 quarantine으로 보낼 해석 실패다. reason은 고정 문구다.
type rejectError struct{ reason string }

func (e *rejectError) Error() string { return "pipeline: " + e.reason }

func reject(reason string) error { return &rejectError{reason: reason} }

// ReasonOf는 quarantine 사유를 꺼낸다. 해석 실패가 아니면 빈 문자열이다.
func ReasonOf(err error) string {
	var r *rejectError
	if errors.As(err, &r) {
		return r.reason
	}
	return ""
}

func header(hs []envelope.Header, key string) ([]byte, bool) {
	var (
		val   []byte
		found bool
	)
	for _, h := range hs {
		if h.Key == key {
			if found {
				return nil, false // 같은 key가 두 번이면 어느 쪽도 믿지 않는다
			}
			val, found = h.Value, true
		}
	}
	return val, found
}

// maxEventIDLen은 event_id 상한이다. log uid(128) + 접두어, metric ID(약 120)보다 넉넉하다.
const maxEventIDLen = 256

// parseMeta는 header를 검증한다. tenant는 key 앞 16B와도 일치해야 한다(ADR 0020 §3).
func parseMeta(m Message) (meta, error) {
	sig, ok := topicSignal[m.Topic]
	if !ok {
		return meta{}, reject(ReasonUnknownTopic)
	}
	get := func(k string) (string, error) {
		v, ok := header(m.Headers, k)
		if !ok || len(v) == 0 {
			return "", reject(ReasonMissingHeader)
		}
		return string(v), nil
	}
	schema, err := get(envelope.HeaderSchema)
	if err != nil {
		return meta{}, err
	}
	if schema != strconv.Itoa(envelope.SchemaVersion) {
		return meta{}, reject(ReasonUnknownSchema) // unknown major는 quarantine (D06 §07)
	}
	tenantStr, err := get(envelope.HeaderTenant)
	if err != nil {
		return meta{}, err
	}
	tenant, err := authz.ParseTenantID(tenantStr)
	if err != nil || tenant.IsZero() {
		return meta{}, reject(ReasonInvalidHeader)
	}
	tb, _ := hex.DecodeString(strings.ReplaceAll(tenant.String(), "-", ""))
	if len(m.Key) != 32 || !bytes.Equal(m.Key[:16], tb) {
		return meta{}, reject(ReasonTenantKeyMismatch)
	}
	signal, err := get(envelope.HeaderSignal)
	if err != nil {
		return meta{}, err
	}
	if Signal(signal) != sig {
		return meta{}, reject(ReasonSignalMismatch)
	}
	eventID, err := get(envelope.HeaderEventID)
	if err != nil {
		return meta{}, err
	}
	if len(eventID) > maxEventIDLen {
		return meta{}, reject(ReasonInvalidHeader)
	}
	ints := map[string]int64{}
	for _, k := range []string{envelope.HeaderReceivedAtMs, envelope.HeaderPolicyVersion, envelope.HeaderRoutingEpoch} {
		s, err := get(k)
		if err != nil {
			return meta{}, err
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return meta{}, reject(ReasonInvalidHeader)
		}
		ints[k] = n
	}
	if ints[envelope.HeaderReceivedAtMs] == 0 {
		return meta{}, reject(ReasonInvalidHeader)
	}
	return meta{
		tenant:        tenant,
		signal:        sig,
		eventID:       eventID,
		receivedAt:    time.UnixMilli(ints[envelope.HeaderReceivedAtMs]).UTC(),
		policyVersion: ints[envelope.HeaderPolicyVersion],
		routingEpoch:  ints[envelope.HeaderRoutingEpoch],
	}, nil
}
