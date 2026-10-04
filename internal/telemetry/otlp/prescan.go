package otlp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"google.golang.org/protobuf/encoding/protowire"
)

// ErrTooComplex: 요소 수나 중첩 깊이가 한도를 넘는다 (413, 배치 분할 안내).
// 바이트 한도만으로는 빈 메시지를 반복한 본문이 decode 후 수십 배 메모리로 증폭되고,
// 깊은 중첩은 decoder 재귀로 stack을 고갈시킨다. decode 전에 본문을 훑어 막는다 (ADR 0017).
var ErrTooComplex = errors.New("otlp: payload too complex")

// pre-scan 한도 (ADR 0017, 설계 가정 — 부하 시험으로 조정).
const (
	// DefaultMaxElements는 요청당 메시지(JSON object·array) 수다.
	// 속성 하나가 KeyValue+AnyValue 2개이므로 일반 SDK batch(512 span)는 수만 개 수준이다.
	DefaultMaxElements = 500_000
	// DefaultMaxDepth는 메시지 중첩 깊이다. OTLP 기본 구조는 protobuf 7~8단, JSON 10단 안팎이다.
	DefaultMaxProtoDepth = 32
	DefaultMaxJSONDepth  = 64
)

// msg는 OTLP protobuf 메시지 종류다. 하위 메시지 필드 번호만 알면 된다(scalar·bytes 필드는 건너뛴다).
type msg uint8

const (
	mUnknown msg = iota
	mTraceReq
	mLogsReq
	mMetricsReq
	mResourceSpans
	mResourceLogs
	mResourceMetrics
	mResource
	mScopeSpans
	mScopeLogs
	mScopeMetrics
	mScope
	mSpan
	mEvent
	mLink
	mStatus
	mLogRecord
	mMetric
	mGauge
	mSum
	mHistogram
	mExpHistogram
	mSummary
	mNumberDP
	mHistogramDP
	mExpHistogramDP
	mExpBuckets
	mSummaryDP
	mQuantile
	mExemplar
	mKeyValue
	mAnyValue
	mArrayValue
	mKeyValueList
	mEntityRef
)

// schema는 opentelemetry-proto(pdata v1.68.0 생성 코드에서 확인)의 하위 메시지 필드 번호다.
// 1000번은 deprecated instrumentation_library_* 필드로 pdata가 scope로 해석한다.
var schema = map[msg]map[protowire.Number]msg{
	mTraceReq:        {1: mResourceSpans},
	mLogsReq:         {1: mResourceLogs},
	mMetricsReq:      {1: mResourceMetrics},
	mResourceSpans:   {1: mResource, 2: mScopeSpans, 1000: mScopeSpans},
	mResourceLogs:    {1: mResource, 2: mScopeLogs, 1000: mScopeLogs},
	mResourceMetrics: {1: mResource, 2: mScopeMetrics, 1000: mScopeMetrics},
	mResource:        {1: mKeyValue, 3: mEntityRef},
	mScopeSpans:      {1: mScope, 2: mSpan},
	mScopeLogs:       {1: mScope, 2: mLogRecord},
	mScopeMetrics:    {1: mScope, 2: mMetric},
	mScope:           {3: mKeyValue},
	mSpan:            {9: mKeyValue, 11: mEvent, 13: mLink, 15: mStatus},
	mEvent:           {3: mKeyValue},
	mLink:            {4: mKeyValue},
	mLogRecord:       {5: mAnyValue, 6: mKeyValue},
	mMetric:          {5: mGauge, 7: mSum, 9: mHistogram, 10: mExpHistogram, 11: mSummary, 12: mKeyValue},
	mGauge:           {1: mNumberDP},
	mSum:             {1: mNumberDP},
	mHistogram:       {1: mHistogramDP},
	mExpHistogram:    {1: mExpHistogramDP},
	mSummary:         {1: mSummaryDP},
	mNumberDP:        {7: mKeyValue, 5: mExemplar},
	mHistogramDP:     {9: mKeyValue, 8: mExemplar},
	mExpHistogramDP:  {1: mKeyValue, 8: mExpBuckets, 9: mExpBuckets, 11: mExemplar},
	mSummaryDP:       {7: mKeyValue, 6: mQuantile},
	mExemplar:        {7: mKeyValue},
	mKeyValue:        {2: mAnyValue},
	mAnyValue:        {5: mArrayValue, 6: mKeyValueList},
	mArrayValue:      {1: mAnyValue},
	mKeyValueList:    {1: mKeyValue},
}

type scanLimits struct {
	maxElements int
	maxDepth    int
}

type protoScanner struct {
	lim      scanLimits
	elements int
}

func requestMsg(sig Signal) msg {
	switch sig {
	case SignalTraces:
		return mTraceReq
	case SignalLogs:
		return mLogsReq
	case SignalMetrics:
		return mMetricsReq
	default:
		return mUnknown
	}
}

// prescanProto는 wire 형식을 한 번 훑어 요소 수·깊이를 센다. 재귀 깊이는 maxDepth로 묶인다.
func prescanProto(b []byte, sig Signal, lim scanLimits) error {
	s := protoScanner{lim: lim}
	return s.walk(b, requestMsg(sig), 1)
}

func (s *protoScanner) walk(b []byte, m msg, depth int) error {
	if depth > s.lim.maxDepth {
		return ErrTooComplex
	}
	fields := schema[m]
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return ErrMalformed
		}
		b = b[n:]
		if typ == protowire.BytesType {
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return ErrMalformed
			}
			b = b[n:]
			if child, ok := fields[num]; ok {
				s.elements++
				if s.elements > s.lim.maxElements {
					return ErrTooComplex
				}
				if err := s.walk(v, child, depth+1); err != nil {
					return err
				}
			}
			continue
		}
		n = protowire.ConsumeFieldValue(num, typ, b)
		if n < 0 {
			return ErrMalformed
		}
		b = b[n:]
	}
	return nil
}

// prescanJSON은 tokenizer로 object·array 수와 깊이를 센다. 값을 메모리에 만들지 않는다.
func prescanJSON(b []byte, lim scanLimits) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	depth, elements := 0, 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return ErrMalformed
		}
		d, ok := tok.(json.Delim)
		if !ok {
			continue
		}
		switch d {
		case '{', '[':
			depth++
			elements++
			if depth > lim.maxDepth || elements > lim.maxElements {
				return ErrTooComplex
			}
		default:
			depth--
		}
	}
}
