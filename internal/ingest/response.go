package ingest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/polynomeer/montracer/internal/telemetry/otlp"
)

// rejectMessage는 partial_success.error_message다. 사유와 건수만 담는다.
func rejectMessage(reasons map[string]int) string {
	if len(reasons) == 0 {
		return ""
	}
	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, reasons[k])
	}
	return "rejected " + strings.Join(parts, ",")
}

func encodeResponse(sig otlp.Signal, enc otlp.Encoding, rejected int64, msg string) ([]byte, error) {
	switch sig {
	case otlp.SignalTraces:
		r := ptraceotlp.NewExportResponse()
		if rejected > 0 {
			r.PartialSuccess().SetRejectedSpans(rejected)
			r.PartialSuccess().SetErrorMessage(msg)
		}
		if enc == otlp.EncodingJSON {
			return r.MarshalJSON()
		}
		return r.MarshalProto()
	case otlp.SignalLogs:
		r := plogotlp.NewExportResponse()
		if rejected > 0 {
			r.PartialSuccess().SetRejectedLogRecords(rejected)
			r.PartialSuccess().SetErrorMessage(msg)
		}
		if enc == otlp.EncodingJSON {
			return r.MarshalJSON()
		}
		return r.MarshalProto()
	default:
		r := pmetricotlp.NewExportResponse()
		if rejected > 0 {
			r.PartialSuccess().SetRejectedDataPoints(rejected)
			r.PartialSuccess().SetErrorMessage(msg)
		}
		if enc == otlp.EncodingJSON {
			return r.MarshalJSON()
		}
		return r.MarshalProto()
	}
}

// grpcCode는 HTTP status에 대응하는 google.rpc.Code다 (OTLP/HTTP 오류 본문).
func grpcCode(status int) int32 {
	switch status {
	case http.StatusBadRequest:
		return 3 // INVALID_ARGUMENT
	case http.StatusUnauthorized:
		return 16 // UNAUTHENTICATED
	case http.StatusForbidden:
		return 7 // PERMISSION_DENIED
	case http.StatusRequestEntityTooLarge:
		return 8 // RESOURCE_EXHAUSTED
	case http.StatusUnsupportedMediaType:
		return 3
	case http.StatusTooManyRequests:
		return 8
	case http.StatusServiceUnavailable:
		return 14 // UNAVAILABLE
	default:
		return 13 // INTERNAL
	}
}

// encodeStatus는 google.rpc.Status{code=1, message=2}를 직렬화한다.
func encodeStatus(enc otlp.Encoding, code int32, message string) []byte {
	if enc == otlp.EncodingJSON {
		b, _ := json.Marshal(map[string]any{"code": code, "message": message})
		return b
	}
	b := protowire.AppendTag(nil, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(code)) //nolint:gosec // grpcCode는 0 이상의 고정 값
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	return protowire.AppendString(b, message)
}
