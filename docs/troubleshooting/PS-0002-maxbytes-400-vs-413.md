# PS-0002: 본문 한도 초과가 413이 아니라 400으로 분류됨

- 날짜: 2026-10-05
- 영역: ingress (`internal/telemetry/otlp`)
- 영향: 큰 batch를 보낸 client가 400(재시도 불가)을 받고 **데이터를 버림** — P1
- 수정: `13ef63f`
- 관련: ADR 0017(한도), ADR 0020 §2(응답 의미), OTLP/HTTP 응답 규약

## 증상

8MiB를 넘는 일반(비압축, 길이 미상) 본문을 보내면 413이 아니라 400이 돌아왔다.

## 원인

1. 본문은 `http.MaxBytesReader`로 감싸 읽는다.
2. 한도를 넘으면 reader가 `*http.MaxBytesError`를 돌려주는데, decode 코드는 이 오류를 일반 읽기 실패(`ErrBodyRead`)로 감쌌다.
3. `ErrBodyRead`는 400으로 매핑되어 있었다. OTLP client는 400을 "데이터가 틀렸다"로 해석해 재시도하지 않는다. 413을 받으면 batch를 나눠 다시 보낼 수 있었던 데이터가 버려진다.
4. 같은 정의 안에서 연결 끊김·읽기 timeout도 400이었다. 데이터가 틀린 것이 아니므로 재시도 가능해야 한다.

## 해결

`errors.As(err, *http.MaxBytesError)`이면 `ErrBodyTooLarge`(413)를 반환한다. `ErrBodyRead`는 재시도 가능한 오류(503)로 정의를 고쳤다.

## 재발 방지

- `internal/ingest/edge_test.go` — chunked(길이 미상) 경로에서 MaxBytesReader가 한도를 넘기면 413
- `internal/telemetry/otlp/otlp_test.go` — 압축 해제 전·후 한도 초과가 `ErrBodyTooLarge`
- `internal/ingest/ingest_test.go` — 큰 본문의 HTTP 상태

## 교훈

오류를 감쌀 때는 **원인 분류를 잃지 않는다.** 표준 라이브러리의 typed error를 먼저 검사한 다음 일반 범주로 감싼다. 수집 경로의 상태 코드는 client의 재시도·분할 동작을 결정하므로, 상태 코드마다 "client가 무엇을 하게 되는가"로 시험한다(ADR 0014).
