// Package otlp는 OTLP/HTTP 요청 본문을 제한된 자원으로 해석하고 record 단위로 검증한다.
//
// 처리 순서의 앞부분을 맡는다 (D02 §04):
// 인증 → **압축 해제 한도 → decode** → tenant 주입 → **속성 검증** → PII 제거 → quota → envelope → Kafka.
// OTLP JSON(hex trace/span ID 등)은 OTel Collector pdata의 공식 구현으로 해석한다 (ADR 0001).
//
// 오류에는 요청 본문 조각을 넣지 않는다. JSON parser 오류는 입력 일부를 포함할 수 있어 버린다.
package otlp

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// Signal은 OTLP 신호 종류다.
type Signal uint8

const (
	SignalTraces Signal = iota + 1
	SignalMetrics
	SignalLogs
)

func (s Signal) String() string {
	switch s {
	case SignalTraces:
		return "traces"
	case SignalMetrics:
		return "metrics"
	case SignalLogs:
		return "logs"
	default:
		return "unknown"
	}
}

// Encoding은 본문 직렬화 형식이다.
type Encoding uint8

const (
	EncodingProtobuf Encoding = iota + 1
	EncodingJSON
)

// 요청 단위 오류. 경계에서 OTLP/HTTP status로 매핑한다 (D02 §04 표).
var (
	// ErrUnsupportedMediaType: Content-Type·Content-Encoding을 지원하지 않는다 (415).
	ErrUnsupportedMediaType = errors.New("otlp: unsupported media type")
	// ErrBodyTooLarge: 압축 전 또는 해제 후 본문이 한도를 넘는다 (413, 배치 분할 안내).
	ErrBodyTooLarge = errors.New("otlp: body too large")
	// ErrMalformed: 본문을 해석할 수 없다 (400). 원인 문자열은 입력 조각을 포함할 수 있어 싣지 않는다.
	ErrMalformed = errors.New("otlp: malformed payload")
	// ErrBodyRead: 본문을 끝까지 읽지 못했다(연결 끊김·읽기 timeout 등). 데이터가 틀린 것이 아니므로
	// 재시도 가능한 오류(503)로 응답한다(ADR 0020 §2).
	ErrBodyRead = errors.New("otlp: body read failed")
)

// Limits는 요청 단위 자원 한도다.
type Limits struct {
	// MaxWireBytes는 전송된(압축된) 본문 한도다.
	MaxWireBytes int64
	// MaxDecodedBytes는 압축 해제 후 본문 한도다. D02 §04: 8MiB.
	MaxDecodedBytes int64
	// MaxElements, MaxProtoDepth, MaxJSONDepth는 decode 전 pre-scan 한도다 (ADR 0017). 0이면 기본값.
	MaxElements   int
	MaxProtoDepth int
	MaxJSONDepth  int
}

// DefaultLimits는 D02 §04의 시작값이다. 압축 전 한도는 해제 후 한도와 같게 둔다(압축되지 않은 요청 기준).
var DefaultLimits = Limits{
	MaxWireBytes:    8 << 20,
	MaxDecodedBytes: 8 << 20,
	MaxElements:     DefaultMaxElements,
	MaxProtoDepth:   DefaultMaxProtoDepth,
	MaxJSONDepth:    DefaultMaxJSONDepth,
}

func orDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// Payload는 해석된 요청이다. Signal에 해당하는 필드 하나만 채워진다.
type Payload struct {
	Signal  Signal
	Traces  ptrace.Traces
	Metrics pmetric.Metrics
	Logs    plog.Logs
}

// ParseEncoding은 Content-Type으로 직렬화 형식을 정한다. OTLP/HTTP는 protobuf와 JSON만 허용한다.
func ParseEncoding(contentType string) (Encoding, error) {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return 0, ErrUnsupportedMediaType
	}
	switch mt {
	case "application/x-protobuf":
		return EncodingProtobuf, nil
	case "application/json":
		return EncodingJSON, nil
	default:
		return 0, ErrUnsupportedMediaType
	}
}

// Decode는 본문을 한도 안에서 읽고 압축을 풀어 signal에 맞게 해석한다.
func Decode(body io.Reader, contentType, contentEncoding string, sig Signal, lim Limits) (Payload, error) {
	enc, err := ParseEncoding(contentType)
	if err != nil {
		return Payload{}, err
	}
	raw, err := readDecoded(body, contentEncoding, lim)
	if err != nil {
		return Payload{}, err
	}
	p := Payload{Signal: sig, Traces: ptrace.NewTraces(), Metrics: pmetric.NewMetrics(), Logs: plog.NewLogs()}
	if len(raw) == 0 {
		// 빈 요청은 record 0건의 정상 요청이다 (OTLP). 형식과 관계없이 같게 처리한다.
		return p, nil
	}
	maxElements := orDefault(lim.MaxElements, DefaultMaxElements)
	if enc == EncodingJSON {
		err = prescanJSON(raw, scanLimits{maxElements: maxElements, maxDepth: orDefault(lim.MaxJSONDepth, DefaultMaxJSONDepth)})
	} else {
		err = prescanProto(raw, sig, scanLimits{maxElements: maxElements, maxDepth: orDefault(lim.MaxProtoDepth, DefaultMaxProtoDepth)})
	}
	if err != nil {
		return Payload{}, err
	}
	switch sig {
	case SignalTraces:
		if enc == EncodingJSON {
			p.Traces, err = (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(raw)
		} else {
			p.Traces, err = (&ptrace.ProtoUnmarshaler{}).UnmarshalTraces(raw)
		}
	case SignalMetrics:
		if enc == EncodingJSON {
			p.Metrics, err = (&pmetric.JSONUnmarshaler{}).UnmarshalMetrics(raw)
		} else {
			p.Metrics, err = (&pmetric.ProtoUnmarshaler{}).UnmarshalMetrics(raw)
		}
	case SignalLogs:
		if enc == EncodingJSON {
			p.Logs, err = (&plog.JSONUnmarshaler{}).UnmarshalLogs(raw)
		} else {
			p.Logs, err = (&plog.ProtoUnmarshaler{}).UnmarshalLogs(raw)
		}
	default:
		return Payload{}, fmt.Errorf("otlp: unknown signal %d", sig)
	}
	if err != nil {
		return Payload{}, ErrMalformed
	}
	return p, nil
}

// readDecoded는 전송 본문과 해제 후 본문을 각각 한도+1 byte까지만 읽는다.
// 한도를 넘는 순간 중단하므로 압축 폭탄도 한도만큼의 메모리·CPU만 쓴다.
func readDecoded(body io.Reader, contentEncoding string, lim Limits) ([]byte, error) {
	wire, err := readAtMost(body, lim.MaxWireBytes)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		if int64(len(wire)) > lim.MaxDecodedBytes {
			return nil, ErrBodyTooLarge
		}
		return wire, nil
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(wire))
		if err != nil {
			return nil, ErrMalformed
		}
		defer func() { _ = zr.Close() }()
		out, err := readAtMost(zr, lim.MaxDecodedBytes)
		if errors.Is(err, ErrBodyTooLarge) {
			return nil, err
		}
		if err != nil {
			return nil, ErrMalformed
		}
		return out, nil
	default:
		return nil, ErrUnsupportedMediaType
	}
}

func readAtMost(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("otlp: limit must be positive")
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		// http.MaxBytesReader가 먼저 한도를 넘겼다 — 크기 초과(413)이지 읽기 실패가 아니다.
		return nil, ErrBodyTooLarge
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBodyRead, err)
	}
	if int64(len(b)) > limit {
		return nil, ErrBodyTooLarge
	}
	return b, nil
}
