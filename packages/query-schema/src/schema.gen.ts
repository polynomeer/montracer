// 생성 파일 — 직접 고치지 않는다. 원천: api/openapi/montracer.yaml (ADR 0059·0061)
// 다시 만들기: pnpm --filter @montracer/query-schema run generate
/* eslint-disable */
export interface paths {
    "/api/v1/traces/{trace_id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * trace 단건 조회 (D02 §13, ADR 0022)
         * @description 명시적 시간 경계(최대 7일)가 필요하다. 보이는 span이 없으면 404다 — 다른 tenant·권한 밖 trace와 없는 trace를 구별하지 않는다.
         *     environment 제한 key는 범위 밖 span을 보지 못하고, 그 자식은 `missing_parent`로 보인다.
         */
        get: operations["getTrace"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/query": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * 공통 검색 (D02 §19, ADR 0037·0043·0060)
         * @description `signal`은 `logs`·`traces`·`metrics`를 실행한다. `errors`는 아직 없어 422다.
         *     `metrics`는 metric 전용 확장(`expression`·`step_seconds`) 본문이고 `/query/metrics`와 같은 query·같은 결과다(D02 §19·§20).
         *     결과 행은 `projection`의 field만 담는다. 누적 10,000행을 넘으면 `meta.warnings`에 `interactive_row_limit_reached`가 붙고 멈춘다.
         */
        post: operations["search"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/query/logs": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * log 검색 (D02 §13, §19, ADR 0037·0039)
         * @description 경로가 `signal=logs`를 정한다. 본문의 `signal`은 생략하거나 `logs`여야 한다. 정렬은 `time desc`만이다.
         */
        post: operations["searchLogs"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/query/traces": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * trace 검색 (D02 §13, §19, ADR 0043)
         * @description 경로가 `signal=traces`를 정한다. 정렬은 `start_time desc`만이다.
         */
        post: operations["searchTraces"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/query/metrics": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * metric 조회 (D02 §13, ADR 0027·0028)
         * @description 비샘플링 metric rollup에서 계산한다. 값이 없는 step은 `v: null`과 `reason`이다 — 0으로 채우지 않는다(계약 6).
         *     histogram percentile은 bucket을 병합해 계산한다(평균하지 않는다, 계약 5). 범위는 최대 7일이다.
         */
        post: operations["queryMetrics"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/metrics": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** metric 사전 (ADR 0046) */
        get: operations["listMetrics"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/metrics/labels": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** metric label key (ADR 0046) */
        get: operations["listMetricLabels"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/services": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * 서비스 목록 (D02 §08, §13, ADR 0038)
         * @description 이름 정규형·environment·service_id 순서다. environment 제한 key는 허용 environment의 서비스만 본다.
         */
        get: operations["listServices"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/services/{service_id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** 서비스 하나 (ADR 0042) */
        get: operations["getService"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/service-map/query": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * service map (D02 §08, §13, ADR 0062)
         * @description span 쌍의 1분 집계(`service_edges_1m`)를 범위만큼 합친 서비스 간 호출이다. 범위는 최대 7일이고 시작은 분 경계로 내린다.
         *     span은 표본일 수 있어 `meta.sampled`는 true이고, 수는 표본 수다. latency는 bucket을 병합한 분위수다(계약 5).
         *     `confidence=estimated`(이름으로만 추정한 서비스)는 `requests`·`errors`·`latency`가 null이다.
         *     볼 수 없는 서비스에 닿는 edge는 결과에 없다. 결과는 500 node·2,000 edge까지이고 나머지 수는 `omitted_count`다.
         *     `meta.warnings`: `cross_environment_edges_hidden`(environment가 다른 서비스 사이 edge를 숨김),
         *     `edge_rows_truncated`(저장소 읽기 상한), `latency_bounds_mismatch`(경계가 다른 window를 합치지 않아 latency 없음),
         *     `range_after_watermark`(범위 끝이 아직 계산하지 않은 window를 포함 — 그 구간의 수는 0이 아니라 미계산이다).
         */
        post: operations["queryServiceMap"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/audit-events": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * 감사 이벤트 조회 (D02 §14, ADR 0034)
         * @description (occurred_at, id) 내림차순. 구간은 최대 366일이다. 범주는 principal 권한으로 제한한다(ADR 0015 §4) — 볼 수 없는 범주를 요청하면 403이다.
         *     첫 page 시각을 snapshot으로 고정해 뒤 page에 새 행이 끼지 않는다. 플랫폼 운영자 행은 actor_id가 `montracer-operator`이고 승인자 ID를 지운다.
         */
        get: operations["listAuditEvents"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/monitors/validate": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * monitor 정의 검증과 24시간 dry-run (D02 §20, ADR 0049, ADR 0052)
         * @description 저장하지 않으므로 Idempotency-Key가 필요 없다. monitors.write(tenant 전체)가 필요하다.
         *     dry-run을 하지 못하면 `dry_run`은 null이고 `data.warnings`에 이유(dry_run_unavailable·dry_run_forbidden·dry_run_failed·dry_run_too_large·dry_run_busy)가 있다 — 그래도 200이다.
         *     형식 오류는 400, 문법은 맞지만 아직 지원하지 않는 의미뿐이면 422, 본문 64 KiB 초과는 413이다.
         */
        post: operations["validateMonitor"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/monitors": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * monitor 목록 (이름순, D02 §14, ADR 0049, ADR 0054)
         * @description monitors.read. 항목마다 지금 상태 요약(`status`)을 싣는다. 상태를 볼 권한(tenant 전체 monitors.read + telemetry.read)이 없으면
         *     `status`가 null이고 `meta.warnings`에 `status_forbidden`이 있다 — 정의 목록은 그대로 200이다.
         */
        get: operations["listMonitors"];
        put?: never;
        /**
         * monitor 생성 (D02 §14, §20, ADR 0049)
         * @description monitors.write(tenant 전체). 같은 Idempotency-Key·같은 본문의 재시도는 처음 응답(201)을 그대로 돌려주고 `Idempotent-Replayed: true`를 붙인다. 다른 본문은 409다.
         *     notification_policy_id가 같은 tenant의 살아 있는 policy가 아니면 400(field `notification_policy_id`)이다. 쓰기 응답의 `status`는 null이다.
         */
        post: operations["createMonitor"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/monitors/{id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description monitor id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        /**
         * monitor 단건 (D02 §14, ADR 0049, ADR 0054)
         * @description monitors.read. 없는·다른 tenant·삭제된 monitor는 같은 404다. 상태 권한이 없으면 `status` null + `status_forbidden` 경고.
         */
        get: operations["getMonitor"];
        /**
         * monitor 수정 (If-Match 필수, D02 §20, ADR 0049)
         * @description monitors.write(tenant 전체). If-Match가 없으면 428, 현재 revision과 다르면 412다. 본문은 생성과 같은 MonitorSpec 전체다. 응답의 `status`는 null이다.
         */
        put: operations["updateMonitor"];
        post?: never;
        /**
         * monitor 삭제 (tombstone, D02 §20, ADR 0049)
         * @description monitors.write(tenant 전체). If-Match는 선택이다 — 있으면 현재 revision이어야 한다(아니면 412).
         */
        delete: operations["deleteMonitor"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/monitors/{id}/groups": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description monitor id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        /**
         * monitor group별 지금 상태 (S09, ADR 0054)
         * @description 나쁜 상태 먼저. cursor가 없고 상한에서 잘리면 `meta.truncated`가 true다. tenant 전체 monitors.read + telemetry.read가 필요하다.
         */
        get: operations["listMonitorGroups"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/monitors/{id}/timeline": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description monitor id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        /**
         * monitor 평가·전이·알림 timeline (S09, ADR 0054)
         * @description 기본은 지난 24시간, 최대 7일(평가 기록 보존). 종류마다 가장 최근 2,000개를 시간순으로 돌려주고 잘리면 `*_truncated`가 true다.
         */
        get: operations["getMonitorTimeline"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/notification-policies": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * notification policy 목록 (이름순, ADR 0053)
         * @description monitors.read(monitor에 붙일 policy 고르기). URL은 scheme·host만 보인다.
         */
        get: operations["listNotificationPolicies"];
        put?: never;
        /**
         * notification policy 생성 (ADR 0053)
         * @description policies.write(tenant 전체). 서버가 만든 서명 secret을 201 응답의 `signing_secret`으로 한 번만 준다.
         *     같은 Idempotency-Key의 재생 응답에는 secret이 없고 `meta.warnings`에 `signing_secret_not_replayed`가 있다. 다른 본문은 409다.
         */
        post: operations["createNotificationPolicy"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/notification-policies/{id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description policy id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        /**
         * notification policy 단건 (ADR 0053)
         * @description monitors.read. 없는·다른 tenant·삭제된 policy는 같은 404다. secret은 없다.
         */
        get: operations["getNotificationPolicy"];
        /**
         * notification policy 수정 (If-Match 필수, ADR 0053, ADR 0056)
         * @description policies.write(tenant 전체). If-Match가 없으면 428, 다르면 412. secret은 바뀌지 않는다.
         *     `webhook`(또는 `"webhook": null`)·`enabled`를 빼면 저장된 값을 그대로 둔다.
         */
        put: operations["updateNotificationPolicy"];
        post?: never;
        /**
         * notification policy 삭제 (tombstone, ADR 0053)
         * @description policies.write(tenant 전체). 이 policy를 쓰는 monitor가 있으면 409다. 보내지 않은 알림은 취소된다. If-Match는 선택이다.
         */
        delete: operations["deleteNotificationPolicy"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/notification-policies/{id}/rotate-secret": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description policy id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * 서명 secret 교체 (Idempotency-Key·If-Match 필수, D02 §20, ADR 0055·0060)
         * @description policies.write(tenant 전체). 새 secret은 이 응답에서만 볼 수 있다.
         *     같은 Idempotency-Key 재요청은 다시 바꾸지 않고 저장된 응답을 돌려준다 — secret 없이 `meta.warnings`에 `signing_secret_not_replayed`.
         *     다른 key로 같은 If-Match를 다시 보내면 412다. 이전 secret은 `previous_secret_expires_in_seconds`(기본·최대 86400, 0이면 바로 폐기)
         *     동안 함께 서명한다. 본문은 비워도 된다.
         */
        post: operations["rotateNotificationPolicySecret"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/notification-policies/{id}/test": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description policy id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * 시험 알림 전송 요청 (D05 §09, ADR 0055)
         * @description policies.write(tenant 전체). 시험 알림을 쌓고 202를 돌려준다. 보내기는 alert-worker가 한다. 결과는 Location(GET …/tests/{test_id})에서 본다.
         *     진행 중인 시험이 있으면 그것을 돌려주고 `meta.warnings`에 `test_in_progress`가 있다. 꺼진 policy에도 보낸다. policy마다 1시간 20번을 넘으면 429다.
         *     같은 Idempotency-Key 재요청은 새로 쌓지 않고 저장된 응답을 그대로 돌려준다(D02 §20, ADR 0060).
         */
        post: operations["sendNotificationPolicyTest"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/notification-policies/{id}/tests/{test_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description policy id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
                /** @description 시험 전송 id (소문자 UUID) */
                test_id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        /**
         * 시험 전송 결과 (ADR 0055)
         * @description policies.write(tenant 전체, 받는 곳의 응답 code가 담겨 있다). 다른 policy·다른 tenant·경보 알림 id는 같은 404다.
         */
        get: operations["getNotificationPolicyTest"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/dashboards": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * dashboard 목록 (제목순, 요약만, ADR 0057)
         * @description dashboards.read(Viewer 이상). 항목에는 spec이 없다(제목·위젯 수·revision).
         */
        get: operations["listDashboards"];
        put?: never;
        /**
         * dashboard 생성 (D02 §14, ADR 0057)
         * @description dashboards.write(Developer 이상). 같은 Idempotency-Key·같은 본문은 처음 응답(201) + `Idempotent-Replayed: true`, 다른 본문은 409다.
         *     형식 오류 400, 아직 지원하지 않는 의미(histogram 위젯·team 변수·or 등 filter)뿐이면 422, 본문 256 KiB 초과 413. 최대 50 위젯.
         */
        post: operations["createDashboard"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/dashboards/{id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description dashboard id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        /**
         * dashboard 단건 (정규형 spec, ADR 0057)
         * @description dashboards.read. 없는·다른 tenant·삭제된 dashboard는 같은 404다.
         */
        get: operations["getDashboard"];
        /**
         * dashboard 수정 (If-Match 필수, D02 §14, ADR 0057)
         * @description dashboards.write. If-Match가 없으면 428, 현재 revision과 다르면 412다. 본문은 DashboardSpec 전체다.
         */
        put: operations["updateDashboard"];
        post?: never;
        /**
         * dashboard 삭제 (tombstone, ADR 0057)
         * @description dashboards.write. If-Match는 선택이다 — 있으면 현재 revision이어야 한다(아니면 412).
         */
        delete: operations["deleteDashboard"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
}
export type webhooks = Record<string, never>;
export interface components {
    schemas: {
        ErrorBody: {
            error: components["schemas"]["Error"];
        };
        /**
         * @description 오류 envelope (D02 §12, §19). SQL·stack·다른 tenant 존재는 담지 않는다.
         *     status와 code: 400·413 INVALID_ARGUMENT, 401 UNAUTHENTICATED, 403 FORBIDDEN, 404 NOT_FOUND, 409 CONFLICT, 410 EXPIRED,
         *     412 REVISION_MISMATCH, 428 INVALID_ARGUMENT(field `If-Match`), 422 INVALID_ARGUMENT·QUERY_BUDGET_EXCEEDED, 429 RATE_LIMITED, 500 INTERNAL, 503 UNAVAILABLE,
         *     504 QUERY_TIMEOUT (계약 시험이 강제한다).
         */
        Error: {
            /** @enum {string} */
            code: "INVALID_ARGUMENT" | "UNAUTHENTICATED" | "FORBIDDEN" | "NOT_FOUND" | "EXPIRED" | "CONFLICT" | "REVISION_MISMATCH" | "RATE_LIMITED" | "UNAVAILABLE" | "QUERY_TIMEOUT" | "QUERY_BUDGET_EXCEEDED" | "INTERNAL";
            message: string;
            request_id: string;
            retryable: boolean;
            /** @description code별 부가 정보. INVALID_ARGUMENT는 `field_violations`, 예산 초과는 한도 값(`max_range_seconds` 등). */
            details?: {
                field_violations?: {
                    field: string;
                    reason: string;
                }[];
            } & {
                [key: string]: unknown;
            };
        };
        /** @description 조회 응답 meta (D02 §19). 계산하지 않는 값은 null이다 — 0이나 정상값으로 채우지 않는다. */
        QueryMeta: {
            request_id: string;
            schema_version: number;
            partial: boolean;
            failed_shards: string[];
            /** Format: date-time */
            watermark: string | null;
            sampled: boolean | null;
            coverage: number | null;
            resolution_seconds: number | null;
            scan_bytes: number | null;
            warnings: string[];
        };
        /** @description 관리 API 응답 meta(조회·쓰기 공통). `warnings`는 처리는 됐지만 알릴 점이고, 없으면 빈 배열이다. */
        ControlMeta: {
            request_id: string;
            schema_version: number;
            warnings: string[];
        };
        /** @description 다음 page cursor. 더 없으면 null. */
        NextCursor: string | null;
        /** @description 소문자 hex 32자 */
        TraceID: string;
        /** @description 소문자 hex 16자 */
        SpanID: string;
        /** @description OTel 속성(타입 보존, key는 속성 이름). JSON에 없는 NaN·±Inf는 문자열 "NaN"·"+Inf"·"-Inf"다. */
        AttributeMap: {
            [key: string]: unknown;
        };
        Trace: {
            trace_id: components["schemas"]["TraceID"];
            spans: components["schemas"]["TraceSpan"][];
            span_count: number;
            /** @description 관측 범위 안의 구조가 완결됐는지. source의 모든 span 수집을 증명하지 않는다 (D02 §22). */
            complete: boolean;
            reasons: ("missing_root" | "missing_parent" | "span_limit_reached" | "payload_undecodable")[];
            /** Format: date-time */
            last_updated_at: string;
        };
        TraceSpan: {
            trace_id: components["schemas"]["TraceID"];
            span_id: components["schemas"]["SpanID"];
            /** @description root면 null */
            parent_span_id: components["schemas"]["SpanID"] | null;
            /** Format: uuid */
            service_id: string;
            service_name: string;
            /** @description resource에 없으면 null */
            environment: string | null;
            name: string;
            kind: string;
            status_code: string;
            status_message: string;
            /** Format: date-time */
            start_time: string;
            /** Format: date-time */
            end_time: string;
            duration_ns: number;
            attributes: components["schemas"]["AttributeMap"];
            resource_attributes: components["schemas"]["AttributeMap"];
            events: {
                name: string;
                /** Format: date-time */
                time: string;
                attributes: components["schemas"]["AttributeMap"];
            }[];
            links: {
                trace_id: components["schemas"]["TraceID"];
                span_id: components["schemas"]["SpanID"];
                attributes: components["schemas"]["AttributeMap"];
            }[];
        };
        /** @description [from, to), RFC3339 UTC */
        TimeRange: {
            /** Format: date-time */
            from: string;
            /** Format: date-time */
            to: string;
        };
        /**
         * @description filter AST (D02 §15). `and`·`or`는 `args`, 나머지는 `field`·`value`를 쓴다. 깊이 4·조건 20·`in` 값 100 이하.
         *     field·연산자는 signal별 catalog가 정한다(log: service_id, service.name, severity_number, trace_id, span_id, body, attributes.<key>).
         */
        FilterNode: {
            /** @enum {string} */
            op: "and" | "or" | "eq" | "neq" | "in" | "exists" | "gt" | "gte" | "lt" | "lte" | "contains";
            args?: components["schemas"]["FilterNode"][];
            field?: string;
            value?: unknown;
        };
        /** @description QuerySpec (D02 §19). `range`를 생략하면 최근 15분이다(다음 page도 첫 page 시각 기준). */
        SearchRequest: {
            /**
             * @description `metrics`는 MetricSearchRequest 본문이다(`POST /query`만).
             * @enum {string}
             */
            signal?: "logs" | "traces" | "errors";
            range?: components["schemas"]["TimeRange"];
            filter?: components["schemas"]["FilterNode"];
            /** @description 결과 field 부분집합. 생략하면 전부. */
            projection?: string[];
            order?: {
                /** @enum {string} */
                field: "time" | "start_time";
                /** @enum {string} */
                direction?: "desc";
            }[];
            limit?: number;
            cursor?: string;
            allow_partial?: boolean;
        };
        /** @description log 결과 행. `projection`의 field만 담는다. 없는 trace·span ID는 null이다. */
        LogRow: {
            /** Format: date-time */
            time?: string;
            event_id?: string;
            /** Format: uuid */
            service_id?: string;
            severity_number?: number;
            trace_id?: components["schemas"]["TraceID"] | null;
            span_id?: components["schemas"]["SpanID"] | null;
            body?: string;
            attributes?: {
                [key: string]: string;
            };
        };
        /** @description log·trace 검색 결과 (D02 §19 공통 성공 응답). 행은 `signal=logs`면 LogRow, `traces`면 TraceRow의 field다. */
        SearchResponse: {
            data: components["schemas"]["SearchRow"][];
            next_cursor: components["schemas"]["NextCursor"];
            meta: components["schemas"]["QueryMeta"];
        };
        /** @description 공통 검색의 metrics 본문 — MetricQueryRequest에 signal=metrics를 더한 것이다. */
        MetricSearchRequest: {
            /** @constant */
            signal: "metrics";
            range: components["schemas"]["TimeRange"];
            /** @description 0이나 생략이면 범위로 정한다 */
            step_seconds?: number;
            expression: components["schemas"]["MetricExpression"];
        };
        /** @description 공통 검색(`POST /query`)의 행. signal에 따라 LogRow 또는 TraceRow의 field만 담는다(`projection` 부분집합). */
        SearchRow: {
            /** Format: date-time */
            time?: string;
            event_id?: string;
            /** Format: uuid */
            service_id?: string;
            severity_number?: number;
            trace_id?: components["schemas"]["TraceID"] | null;
            span_id?: components["schemas"]["SpanID"] | null;
            body?: string;
            attributes?: {
                [key: string]: string;
            };
            /** Format: date-time */
            start_time?: string;
            duration_ms?: number;
            span_count?: number;
            has_error?: boolean;
            complete?: boolean;
            reasons?: ("missing_root" | "missing_parent" | "span_limit_reached")[];
            root_service?: string | null;
            /** Format: uuid */
            root_service_id?: string | null;
            root_name?: string | null;
            /** Format: date-time */
            last_updated_at?: string;
        };
        /** @description trace 검색 결과 행(trace 요약). `projection`의 field만 담는다. */
        TraceRow: {
            trace_id?: components["schemas"]["TraceID"];
            /** Format: date-time */
            start_time?: string;
            duration_ms?: number;
            span_count?: number;
            has_error?: boolean;
            complete?: boolean;
            reasons?: ("missing_root" | "missing_parent" | "span_limit_reached")[];
            /** @description 서비스 catalog에 없으면 null */
            root_service?: string | null;
            /** Format: uuid */
            root_service_id?: string | null;
            /** @description root span이 없으면 null */
            root_name?: string | null;
            /** Format: date-time */
            last_updated_at?: string;
        };
        MetricQueryRequest: {
            range: components["schemas"]["TimeRange"];
            /** @description 0이나 생략이면 범위로 정한다 */
            step_seconds?: number;
            expression: components["schemas"]["MetricExpression"];
        };
        /**
         * @description metric 연산. counter는 rate·increase·sum, gauge는 avg·min·max, histogram은 percentile·count·hist_sum (ADR 0025·0042)
         * @enum {string}
         */
        MetricAggregation: "rate" | "increase" | "sum" | "avg" | "min" | "max" | "count" | "hist_sum" | "p50" | "p90" | "p95" | "p99";
        /** @description `filter`는 label 조건이다. 지금은 `and`와 `eq`만 실행한다(나머지는 422). */
        MetricExpression: {
            metric: string;
            aggregation: components["schemas"]["MetricAggregation"];
            filter?: components["schemas"]["FilterNode"];
            group_by?: string[];
        };
        MetricQueryResponse: {
            data: {
                series: components["schemas"]["MetricSeries"][];
                /**
                 * @description 계산에 쓴 rollup 해상도
                 * @enum {integer}
                 */
                source_window_seconds: 60 | 3600;
                range: components["schemas"]["TimeRange"];
            };
            meta: components["schemas"]["QueryMeta"];
        };
        MetricSeries: {
            labels: {
                [key: string]: string;
            };
            /** @description 단위가 섞였거나 모르면 null */
            unit: string | null;
            points: {
                /** Format: date-time */
                t: string;
                /** @description 값이 없으면 null(이유는 reason) */
                v: number | null;
                /** @enum {string} */
                reason?: "no_data" | "missing_baseline" | "not_applicable" | "bounds_mismatch" | "unit_conflict" | "type_conflict" | "pending";
                partial: boolean;
            }[];
            /** @description 값이 있는 step / 전체 step */
            completeness: number;
            missing: components["schemas"]["TimeRange"][];
            flags: string[];
        };
        MetricItem: {
            name: string;
            /** @description 같은 이름에 유형·단위가 다른 변형이 있다 */
            conflict: boolean;
            variants: {
                type: string;
                temporality: string;
                monotonic: boolean;
                unit: string;
                series: number;
                /** Format: date-time */
                last_seen: string;
                /** @description 이 유형에 쓸 수 있는 연산(rollup 조회가 지원하지 않는 유형이면 빈 배열) */
                aggregations: components["schemas"]["MetricAggregation"][];
            }[];
        };
        MetricLabelItem: {
            key: string;
            sources: string[];
            series: number;
        };
        /** @description 서비스 catalog 항목 (D02 §08). 모르는 값·사용자가 정하지 않은 값은 null이다. */
        Service: {
            /** Format: uuid */
            service_id: string;
            name: string;
            namespace: string;
            environment: string;
            language: string | null;
            /**
             * @description inactive는 24시간, archived는 30일 넘게 보지 못했다
             * @enum {string}
             */
            status: "active" | "inactive" | "archived";
            /** Format: date-time */
            first_seen: string;
            /** Format: date-time */
            last_seen: string;
            owner_team: string | null;
            repository_url: string | null;
            runbook_url: string | null;
            tier: string | null;
            tags: string[];
        };
        ServiceMapRequest: {
            range: components["schemas"]["TimeRange"];
            /** @description 이 environment의 서비스에서 나가는 edge만 */
            environment?: string;
            /** @description 이 서비스들에 닿는 edge만(source 또는 destination) */
            service_ids?: components["schemas"]["LowercaseUUID"][];
        };
        ServiceMapResponse: {
            data: {
                nodes: components["schemas"]["MapNode"][];
                edges: components["schemas"]["MapEdge"][];
                /** @description 상한(500 node·2,000 edge)을 넘어 뺀 수 */
                omitted_count: {
                    nodes: number;
                    edges: number;
                };
                range: components["schemas"]["TimeRange"];
            };
            meta: components["schemas"]["QueryMeta"];
        };
        /**
         * @description `service`는 catalog 서비스(`node_id` = service_id)다. 나머지는 계측되지 않은 상대이고 environment마다 다른 node다
         *     (`node_id` = `db:<system>[/<namespace>]@<env>`, `queue:<system>@<env>`, `host:<등록 domain>|ip|internal@<env>`, `peer:<이름>@<env>`).
         *     외부 host는 전체 이름·IP를 담지 않는다. catalog와 맞지 않는 내부 host 이름은 `host:internal` 하나로 접는다.
         */
        MapNode: {
            node_id: string;
            /** @enum {string} */
            kind: "service" | "database" | "queue" | "external";
            name: string;
            /** Format: uuid */
            service_id: string | null;
            namespace: string | null;
            environment: string;
        };
        /** @description bucket 병합 분위수(ms). 경계 안 선형 보간이고, 마지막 경계를 넘으면 그 경계다. */
        MapLatency: {
            p50_ms: number | null;
            p95_ms: number | null;
            p99_ms: number | null;
        };
        MapEdge: {
            /** @description source node_id */
            source: string;
            /** @description destination node_id */
            destination: string;
            /** @enum {string} */
            connection: "rpc" | "messaging";
            /**
             * @description observed는 span 짝·client span 속성으로 본 호출, estimated는 이름(peer.service·내부 host)만 맞춘 서비스
             * @enum {string}
             */
            confidence: "observed" | "estimated";
            /** @description 표본 호출 수. estimated는 null */
            requests: number | null;
            errors: number | null;
            /** @description rpc는 client가 본 시간, messaging은 consumer 처리 시간(짝이 없으면 publish 시간). 관측이 없거나 estimated면 null */
            latency: components["schemas"]["MapLatency"] | null;
            /** @description messaging의 큐 대기(consumer 시작 − producer 끝). 그 밖은 null */
            queue_wait: components["schemas"]["MapLatency"] | null;
            /** Format: date-time */
            last_seen: string;
        };
        /** @description 소문자 UUID */
        LowercaseUUID: string;
        /** @description label key → 값 */
        LabelMap: {
            [key: string]: string;
        };
        /** @description group 상태 → group 수 */
        StateCountMap: {
            [key: string]: number;
        };
        /** @description 임의 JSON 객체 */
        JSONObject: {
            [key: string]: components["schemas"]["JSONValue"];
        };
        /** @description 임의 JSON 값 (객체 key는 모두 허용) */
        JSONValue: {
            [key: string]: components["schemas"]["JSONValue"];
        };
        /** @description 감사 이벤트 (ADR 0034). details는 작성 시 PII·secret을 넣지 않은 저장값 그대로다. */
        AuditEvent: {
            id: string;
            /** Format: date-time */
            occurred_at: string;
            /** @enum {string} */
            category: "operations" | "security";
            action: string;
            /** @enum {string} */
            actor_kind: "user" | "api_key" | "ingest_key" | "system" | "operator";
            /** @description operator 행은 `montracer-operator` */
            actor_id: string;
            resource_type: string;
            resource_id: string;
            request_id: string | null;
            details: components["schemas"]["JSONObject"];
        };
        /** @description 정규형 label 조건 AST (and / eq만, 깊이 4, leaf 20). 빈 field·value·args는 생략된다. */
        LabelFilter: {
            /** @enum {string} */
            op: "and" | "eq";
            field?: string;
            value?: string;
            args?: components["schemas"]["LabelFilter"][];
        };
        /** @description 요청의 label 조건 AST. and는 args만, eq는 field와 value. 다른 op(or·not·in 등)는 422다. */
        LabelFilterInput: {
            op: string;
            field?: string;
            value?: string | null;
            args?: components["schemas"]["LabelFilterInput"][] | null;
        };
        /**
         * @description MonitorSpec 요청 (D02 §14, ADR 0049). 모르는 field는 400이다. 정규형(MonitorSpec)을 그대로 다시 보낼 수 있다.
         *     종류별 규칙(error_ratio의 threshold 0~1·aggregation 금지·minimum_requests, window·for·after_seconds의 evaluation 배수)은 서버가 검증한다.
         */
        MonitorSpecInput: {
            /** @description 앞뒤 공백을 뺀 1..200자 */
            name: string;
            /** @description 기본 true */
            enabled?: boolean | null;
            query: {
                /** @enum {string} */
                kind: "metric" | "error_ratio";
                /** @description metric은 필수(1..256 byte). error_ratio는 생략 또는 http.server.request.duration */
                metric?: string | null;
                /**
                 * @description metric만 (필수). error_ratio는 null이거나 생략
                 * @enum {string|null}
                 */
                aggregation?: "rate" | "increase" | "sum" | "avg" | "min" | "max" | "count" | "hist_sum" | "p50" | "p90" | "p95" | "p99" | null;
                filter?: components["schemas"]["LabelFilterInput"] | null;
                group_by?: string[] | null;
            };
            window_seconds: number;
            /** @enum {integer} */
            evaluation_seconds: 30 | 60 | 300;
            condition: {
                /** @enum {string} */
                operator: "gt" | "gte" | "lt" | "lte";
                threshold: number;
            };
            /** @description error_ratio만 (기본 100) */
            minimum_requests?: number | null;
            for_seconds: number;
            /**
             * @description D02 §17로 2 고정. 다른 값은 422
             * @enum {integer|null}
             */
            recovery_evaluations?: 2 | null;
            /** @description 결측 정책 (필수, 결측을 OK로 보는 선택지는 없다). 문자열 "no_data" / "alert_after_<초>s" 또는 객체 */
            no_data: string | {
                /** @enum {string} */
                action: "no_data" | "alert";
                /** @description alert만 (필수, window_seconds..86400) */
                after_seconds?: number | null;
            };
            notification_policy_id?: components["schemas"]["LowercaseUUID"] | null;
        };
        /** @description 정규형 MonitorSpec. 기본값을 모두 채운 명시적 JSON이다 (ADR 0049). */
        MonitorSpec: {
            name: string;
            enabled: boolean;
            query: {
                /** @enum {string} */
                kind: "metric" | "error_ratio";
                metric: string;
                /**
                 * @description error_ratio는 null
                 * @enum {string|null}
                 */
                aggregation: "rate" | "increase" | "sum" | "avg" | "min" | "max" | "count" | "hist_sum" | "p50" | "p90" | "p95" | "p99" | null;
                filter: components["schemas"]["LabelFilter"] | null;
                group_by: string[];
            };
            window_seconds: number;
            /** @enum {integer} */
            evaluation_seconds: 30 | 60 | 300;
            condition: {
                /** @enum {string} */
                operator: "gt" | "gte" | "lt" | "lte";
                threshold: number;
            };
            /** @description error_ratio만, 그 밖은 null */
            minimum_requests: number | null;
            for_seconds: number;
            recovery_evaluations: number;
            no_data: {
                /** @enum {string} */
                action: "no_data" | "alert";
                /** @description alert만, 그 밖은 null */
                after_seconds: number | null;
            };
            notification_policy_id: components["schemas"]["LowercaseUUID"] | null;
        };
        Monitor: {
            id: components["schemas"]["LowercaseUUID"];
            name: string;
            enabled: boolean;
            revision: number;
            spec: components["schemas"]["MonitorSpec"];
            /** Format: date-time */
            created_at: string;
            /** Format: date-time */
            updated_at: string;
            created_by: string;
            updated_by: string;
            /** @description 지금 상태 요약 (목록·단건만). 쓰기 응답과 상태 권한이 없을 때(status_forbidden)는 null */
            status: components["schemas"]["MonitorStatus"] | null;
        };
        /** @description 상태 요약. 평가된 적 없음·꺼짐·결측·오류를 OK와 구분한다 (계약 6). stale은 평가가 멈췄다는 뜻이다. */
        MonitorStatus: {
            /** @enum {string} */
            state: "ALERT" | "EVALUATION_ERROR" | "NO_DATA" | "RECOVERING" | "PENDING" | "OK" | "NOT_EVALUATED" | "DISABLED";
            groups_by_state: components["schemas"]["StateCountMap"];
            last_evaluation: components["schemas"]["MonitorEvaluation"] | null;
            stale: boolean;
        };
        MonitorEvaluation: {
            /** Format: date-time */
            slot: string;
            /** @enum {string} */
            status: "evaluated" | "no_data" | "error";
            reason: string | null;
            /** Format: date-time */
            window_start: string | null;
            /** Format: date-time */
            window_end: string | null;
            groups: number;
            alerting: number;
            duration_ms: number;
        };
        MonitorGroup: {
            group_hash: string;
            labels: components["schemas"]["LabelMap"];
            /** @enum {string} */
            state: "OK" | "PENDING" | "ALERT" | "RECOVERING" | "NO_DATA" | "EVALUATION_ERROR";
            episode_id: string | null;
            episode_reason: string | null;
            /** Format: date-time */
            violation_since: string | null;
            /** Format: date-time */
            nodata_since: string | null;
            /** Format: date-time */
            last_window_end: string | null;
            last_value: number | null;
            last_reason: string | null;
            /** Format: date-time */
            updated_at: string;
        };
        /** @description ControlMeta + truncated (상한에서 잘렸는지) */
        MonitorGroupsMeta: {
            request_id: string;
            schema_version: number;
            warnings: string[];
            truncated: boolean;
        };
        MonitorTransition: {
            /** Format: date-time */
            at: string;
            group_hash: string;
            labels: components["schemas"]["LabelMap"];
            from: string;
            /** @description 정의 변경으로 끝난 상태는 null */
            to: string | null;
            opened: boolean;
            closed: boolean;
            episode_id: string | null;
            reason: string | null;
            value: number | null;
        };
        MonitorNotification: {
            id: string;
            policy_id: string;
            /**
             * @description 지금 값은 `alert.opened`·`alert.resolved`·`alert.renotified`다. 이전 control-api(롤백)는 재알림을 저장 종류
             *     그대로(`alert.renotified.<n>`) 줄 수 있다 — client는 모르는 값을 원문으로 보인다.
             */
            event_type: string;
            /** @description 재알림 번호(1..3), 그 밖은 null */
            renotify_sequence: number | null;
            group_hash: string;
            episode_id: string;
            /** @enum {string} */
            status: "pending" | "delivered" | "failed" | "canceled";
            attempts: number;
            last_status_code: number | null;
            last_error: string | null;
            /** Format: date-time */
            created_at: string;
            /** Format: date-time */
            finished_at: string | null;
        };
        MonitorTimeline: {
            /** Format: date-time */
            from: string;
            /** Format: date-time */
            to: string;
            evaluations: components["schemas"]["MonitorEvaluation"][];
            transitions: components["schemas"]["MonitorTransition"][];
            notifications: components["schemas"]["MonitorNotification"][];
            evaluations_truncated: boolean;
            transitions_truncated: boolean;
            notifications_truncated: boolean;
        };
        /** @description 지난 24시간 재평가. evaluations가 0이면 reason이 있다. 결측·오류를 발화 없음과 섞지 않는다 (계약 6). */
        MonitorDryRun: {
            /**
             * Format: date-time
             * @description 첫 평가 시점(window 끝)
             */
            from: string | null;
            /**
             * Format: date-time
             * @description 마지막 평가 시점
             */
            to: string | null;
            warmup_seconds: number;
            /**
             * Format: date-time
             * @description rollup watermark
             */
            data_until: string | null;
            step_seconds: number;
            evaluations: number;
            coverage: {
                evaluated: number;
                no_data: number;
                error: number;
            };
            groups: components["schemas"]["MonitorDryRunGroup"][];
            groups_total: number;
            groups_omitted: number;
            /** @enum {string} */
            reason: "" | "no_watermark" | "stale_watermark" | "no_data";
        };
        MonitorDryRunGroup: {
            group_key: string;
            labels: components["schemas"]["LabelMap"];
            firing_intervals: {
                /** Format: date-time */
                start: string;
                /**
                 * Format: date-time
                 * @description dry-run 끝까지 열려 있으면 null
                 */
                end: string | null;
                /** @description violation | no_data */
                reason: string;
            }[];
            firing_intervals_omitted: number;
            firing_seconds: number;
            /** @enum {string} */
            final_state: "OK" | "PENDING" | "ALERT" | "RECOVERING" | "NO_DATA" | "EVALUATION_ERROR";
            evaluated: number;
            no_data: number;
            partial: number;
            /** Format: date-time */
            first_value_at: string | null;
            max_value: number | null;
            last_value: number | null;
            /** @description 마지막 시점에 값이 없던 이유. 값이 있었으면 빈 문자열 */
            last_reason: string;
            transitions: number;
        };
        /** @description 생성 요청. 모르는 field는 400이다. 서명 secret은 서버가 만든다. */
        NotificationPolicyCreate: {
            /** @description 앞뒤 공백을 뺀 1..200자 */
            name: string;
            webhook: {
                /** @description https 절대 URL. 공인 주소만, 자격 증명·fragment 금지 (SSRF 방어) */
                url: string;
            };
            /** @description 기본 true */
            enabled?: boolean | null;
        };
        /** @description 수정 요청. webhook·enabled를 빼거나 null이면 저장된 값을 그대로 둔다 (ADR 0056). */
        NotificationPolicyUpdate: {
            /** @description 앞뒤 공백을 뺀 1..200자 */
            name: string;
            webhook?: {
                url: string;
            } | null;
            enabled?: boolean | null;
        };
        RotateSecretRequest: {
            /** @description 이전 secret을 함께 서명할 기간 (기본 86400, 0이면 바로 폐기) */
            previous_secret_expires_in_seconds?: number | null;
        } | null;
        NotificationPolicy: {
            id: components["schemas"]["LowercaseUUID"];
            name: string;
            enabled: boolean;
            revision: number;
            /** @description URL은 scheme·host만 보인다(path·query가 있으면 `/…`). 교체 겹침 기간에는 이전 secret id와 만료 시각이 있다. */
            webhook: {
                url: string;
                signing_key_id: string;
                previous_signing_key_id: string | null;
                /** Format: date-time */
                previous_signing_key_expires_at: string | null;
            };
            /** Format: date-time */
            created_at: string;
            /** Format: date-time */
            updated_at: string;
            created_by: string;
            updated_by: string;
        };
        NotificationPolicyResponse: {
            data: components["schemas"]["NotificationPolicy"];
            /** @description 생성(재생 제외)·secret 교체 응답에만 있다. 다시 볼 수 없다. */
            signing_secret?: string;
            meta: components["schemas"]["ControlMeta"];
        };
        NotificationTest: {
            id: components["schemas"]["LowercaseUUID"];
            policy_id: string;
            /** @enum {string} */
            status: "pending" | "delivered" | "failed" | "canceled";
            attempts: number;
            last_status_code: number | null;
            last_error: string | null;
            /** Format: date-time */
            created_at: string;
            /** Format: date-time */
            finished_at: string | null;
        };
        /**
         * @description DashboardSpec 요청 (D05 §11). 모르는 field는 400이다. 정규형(DashboardSpec)을 그대로 다시 보낼 수 있다.
         *     위젯마다 layout 하나, 24열 안, 겹치지 않음, 변수 참조("$이름")는 그 변수 종류의 field eq에만 — 서버가 검증한다.
         */
        DashboardSpecInput: {
            /** @enum {integer|null} */
            version?: 1 | null;
            /** @description 앞뒤 공백을 뺀 1..200자 */
            title: string;
            /** @description 0..2000자 */
            description?: string | null;
            variables?: {
                name: string;
                /**
                 * @description team은 422
                 * @enum {string}
                 */
                kind: "environment" | "service";
                /** @description null이면 전체 */
                default?: string | null;
            }[] | null;
            layout?: {
                widget_id: string;
                x: number;
                y: number;
                w: number;
                h: number;
            }[] | null;
            widgets?: {
                id: string;
                /**
                 * @description histogram은 422
                 * @enum {string}
                 */
                kind: "time_series" | "stat" | "table" | "top_list" | "text";
                title?: string | null;
                unit?: string | null;
                /** @description 종류가 쓰지 않는 field는 400 (time_series stack, stat decimals, table·top_list decimals·limit·sort) */
                display?: {
                    stack?: boolean | null;
                    decimals?: number | null;
                    limit?: number | null;
                    /** @enum {string|null} */
                    sort?: "desc" | "asc" | null;
                } | null;
                time_override?: {
                    /** @enum {string} */
                    range: "15m" | "1h" | "4h" | "1d" | "7d";
                } | null;
                /** @description text 위젯은 없어야 하고 그 밖은 필수 */
                query?: {
                    metric?: string | null;
                    /** @enum {string|null} */
                    aggregation?: "rate" | "increase" | "sum" | "avg" | "min" | "max" | "count" | "hist_sum" | "p50" | "p90" | "p95" | "p99" | null;
                    filter?: components["schemas"]["LabelFilterInput"] | null;
                    group_by?: string[] | null;
                } | null;
                /** @description text 위젯만 (1..10000자, 글자 그대로 표시) */
                text?: string | null;
            }[] | null;
        };
        /** @description 정규형 DashboardSpec. 기본값을 모두 채운다. */
        DashboardSpec: {
            /** @enum {integer} */
            version: 1;
            title: string;
            description: string;
            variables: {
                name: string;
                /** @enum {string} */
                kind: "environment" | "service";
                default: string | null;
            }[];
            layout: {
                widget_id: string;
                x: number;
                y: number;
                w: number;
                h: number;
            }[];
            widgets: components["schemas"]["DashboardWidget"][];
        };
        DashboardWidget: {
            id: string;
            /** @enum {string} */
            kind: "time_series" | "stat" | "table" | "top_list" | "text";
            title: string;
            unit: string | null;
            /** @description 그 종류가 쓰는 field만 있다 (time_series stack, stat decimals, table·top_list decimals·limit·sort, text 없음) */
            display: {
                stack?: boolean;
                decimals?: number;
                limit?: number;
                /** @enum {string} */
                sort?: "desc" | "asc";
            };
            time_override: {
                /** @enum {string} */
                range: "15m" | "1h" | "4h" | "1d" | "7d";
            } | null;
            /** @description text 위젯은 null */
            query: {
                metric: string;
                aggregation: components["schemas"]["MetricAggregation"];
                filter: components["schemas"]["LabelFilter"] | null;
                group_by: string[];
            } | null;
            /** @description text 위젯만 */
            text: string | null;
        };
        /** @description 목록 항목 (spec 없음) */
        DashboardSummary: {
            id: components["schemas"]["LowercaseUUID"];
            title: string;
            revision: number;
            widget_count: number;
            /** Format: date-time */
            created_at: string;
            /** Format: date-time */
            updated_at: string;
            created_by: string;
            updated_by: string;
        };
        Dashboard: {
            id: components["schemas"]["LowercaseUUID"];
            title: string;
            revision: number;
            widget_count: number;
            spec: components["schemas"]["DashboardSpec"];
            /** Format: date-time */
            created_at: string;
            /** Format: date-time */
            updated_at: string;
            created_by: string;
            updated_by: string;
        };
    };
    responses: {
        /** @description log 검색 결과 */
        LogSearchResult: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": {
                    data: components["schemas"]["LogRow"][];
                    next_cursor: components["schemas"]["NextCursor"];
                    meta: components["schemas"]["QueryMeta"];
                };
            };
        };
        /** @description trace 검색 결과 */
        TraceSearchResult: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": {
                    data: components["schemas"]["TraceRow"][];
                    next_cursor: components["schemas"]["NextCursor"];
                    meta: components["schemas"]["QueryMeta"];
                };
            };
        };
        /** @description 타입·문법 오류. `details.field_violations`에 field 경로가 있다 (INVALID_ARGUMENT). */
        BadRequest: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorBody"];
            };
        };
        /** @description 인증 실패 (UNAUTHENTICATED) */
        Unauthorized: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorBody"];
            };
        };
        /** @description 작업 권한 없음 (FORBIDDEN) */
        Forbidden: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorBody"];
            };
        };
        /** @description 없거나 볼 수 없는 자원 (NOT_FOUND). 다른 tenant·권한 밖과 구별하지 않는다. */
        NotFound: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorBody"];
            };
        };
        /** @description 상태 충돌, 같은 Idempotency-Key의 다른 본문 (CONFLICT) */
        Conflict: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorBody"];
            };
        };
        /** @description If-Match revision 불일치 (REVISION_MISMATCH). 최신을 다시 읽는다. */
        PreconditionFailed: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorBody"];
            };
        };
        /** @description If-Match가 없다 (INVALID_ARGUMENT, `details.field_violations[].field` = `If-Match`). revision 불일치(412)와 code로 구별된다. */
        PreconditionRequired: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorBody"];
            };
        };
        /** @description 본문 크기 초과 */
        PayloadTooLarge: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorBody"];
            };
        };
        /** @description 지원하지 않는 의미, 실행 예산 초과 (INVALID_ARGUMENT, QUERY_BUDGET_EXCEEDED) */
        Unprocessable: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorBody"];
            };
        };
        /** @description tenant 동시 실행·quota 초과 (RATE_LIMITED) */
        TooManyRequests: {
            headers: {
                "Retry-After": components["headers"]["RetryAfter"];
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorBody"];
            };
        };
        /** @description 서버 결함 (INTERNAL) */
        Internal: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorBody"];
            };
        };
        /** @description 의존 서비스 실패 (UNAVAILABLE). `retryable`을 본다. */
        Unavailable: {
            headers: {
                "Retry-After": components["headers"]["RetryAfter"];
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorBody"];
            };
        };
        /** @description 조회 시간 초과 (QUERY_TIMEOUT) */
        QueryTimeout: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorBody"];
            };
        };
    };
    parameters: {
        /** @description 구간 시작(포함), RFC3339 UTC */
        From: string;
        /** @description 구간 끝(제외), RFC3339 UTC */
        To: string;
        /** @description page 크기 (기본 100, 최대 1,000) */
        Limit: number;
        /** @description 앞 응답의 `next_cursor`(서명, 15분). 같은 조건·같은 권한에서만 쓸 수 있다. */
        Cursor: string;
        /** @description 쓰기 POST의 멱등 key(24시간). 같은 key에 다른 본문은 409다 (D02 §20). */
        IdempotencyKey: string;
        /** @description 읽은 revision의 ETag. 없으면 428, 다르면 412다 (D02 §20). */
        IfMatch: string;
    };
    requestBodies: never;
    headers: {
        /** @description 현재 revision. 수정·삭제의 If-Match에 그대로 쓴다. */
        ETag: string;
        /** @description 만든 자원의 경로 */
        Location: string;
        /** @description 다시 시도할 때까지 기다릴 초 */
        RetryAfter: number;
        /**
         * @description 같은 Idempotency-Key의 저장된 응답을 다시 돌려줬으면 `true`. 본문은 처음 응답 그대로라 `meta.request_id`는 처음 요청의 값이고,
         *     header `X-Request-ID`는 이 요청의 값이다 (ADR 0060 §5).
         */
        IdempotentReplayed: "true";
    };
    pathItems: never;
}
export type $defs = Record<string, never>;
export interface operations {
    getTrace: {
        parameters: {
            query: {
                /** @description 구간 시작(포함), RFC3339 UTC */
                from: components["parameters"]["From"];
                /** @description 구간 끝(제외), RFC3339 UTC */
                to: components["parameters"]["To"];
                /** @description 이 서비스의 span만 */
                service_id?: string;
            };
            header?: never;
            path: {
                trace_id: components["schemas"]["TraceID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description trace */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["Trace"];
                        meta: components["schemas"]["QueryMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            422: components["responses"]["Unprocessable"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
            504: components["responses"]["QueryTimeout"];
        };
    };
    search: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["SearchRequest"] | components["schemas"]["MetricSearchRequest"];
            };
        };
        responses: {
            /** @description 검색 결과. log·trace는 행 목록과 cursor, metrics는 `/query/metrics`와 같은 series다. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["SearchResponse"] | components["schemas"]["MetricQueryResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            413: components["responses"]["PayloadTooLarge"];
            422: components["responses"]["Unprocessable"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
            504: components["responses"]["QueryTimeout"];
        };
    };
    searchLogs: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["SearchRequest"];
            };
        };
        responses: {
            200: components["responses"]["LogSearchResult"];
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            413: components["responses"]["PayloadTooLarge"];
            422: components["responses"]["Unprocessable"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
            504: components["responses"]["QueryTimeout"];
        };
    };
    searchTraces: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["SearchRequest"];
            };
        };
        responses: {
            200: components["responses"]["TraceSearchResult"];
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            413: components["responses"]["PayloadTooLarge"];
            422: components["responses"]["Unprocessable"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
            504: components["responses"]["QueryTimeout"];
        };
    };
    queryMetrics: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["MetricQueryRequest"];
            };
        };
        responses: {
            /** @description series */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["MetricQueryResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            413: components["responses"]["PayloadTooLarge"];
            422: components["responses"]["Unprocessable"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
            504: components["responses"]["QueryTimeout"];
        };
    };
    listMetrics: {
        parameters: {
            query: {
                /** @description 구간 시작(포함), RFC3339 UTC */
                from: components["parameters"]["From"];
                /** @description 구간 끝(제외), RFC3339 UTC */
                to: components["parameters"]["To"];
                /** @description 이름 부분 문자열(대소문자 무시) */
                q?: string;
                /** @description page 크기 (기본 200, 최대 1,000) */
                limit?: number;
                /** @description 앞 응답의 `next_cursor`(서명, 15분). 같은 조건·같은 권한에서만 쓸 수 있다. */
                cursor?: components["parameters"]["Cursor"];
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description metric 이름과 유형별 변형 */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["MetricItem"][];
                        next_cursor: components["schemas"]["NextCursor"];
                        meta: components["schemas"]["QueryMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            422: components["responses"]["Unprocessable"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
            504: components["responses"]["QueryTimeout"];
        };
    };
    listMetricLabels: {
        parameters: {
            query: {
                metric: string;
                /** @description 구간 시작(포함), RFC3339 UTC */
                from: components["parameters"]["From"];
                /** @description 구간 끝(제외), RFC3339 UTC */
                to: components["parameters"]["To"];
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description label key */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: {
                            metric: string;
                            keys: components["schemas"]["MetricLabelItem"][];
                        };
                        meta: components["schemas"]["QueryMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            422: components["responses"]["Unprocessable"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
            504: components["responses"]["QueryTimeout"];
        };
    };
    listServices: {
        parameters: {
            query?: {
                environment?: string;
                /** @description owner_team */
                owner?: string;
                /** @description 30일 넘게 보지 못한 서비스도 (기본 false) */
                include_archived?: boolean;
                /** @description page 크기 (기본 100, 최대 1,000) */
                limit?: components["parameters"]["Limit"];
                /** @description 앞 응답의 `next_cursor`(서명, 15분). 같은 조건·같은 권한에서만 쓸 수 있다. */
                cursor?: components["parameters"]["Cursor"];
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description 서비스 */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["Service"][];
                        next_cursor: components["schemas"]["NextCursor"];
                        meta: components["schemas"]["QueryMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
            504: components["responses"]["QueryTimeout"];
        };
    };
    getService: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                service_id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description 서비스 */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["Service"];
                        meta: components["schemas"]["QueryMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
            504: components["responses"]["QueryTimeout"];
        };
    };
    queryServiceMap: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ServiceMapRequest"];
            };
        };
        responses: {
            /** @description node와 edge */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ServiceMapResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            413: components["responses"]["PayloadTooLarge"];
            422: components["responses"]["Unprocessable"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
            504: components["responses"]["QueryTimeout"];
        };
    };
    listAuditEvents: {
        parameters: {
            query: {
                /** @description 구간 시작(포함), RFC3339 UTC */
                from: components["parameters"]["From"];
                /** @description 구간 끝(제외), RFC3339 UTC */
                to: components["parameters"]["To"];
                /** @description 이 범주만. 없으면 볼 수 있는 범주 전체 */
                category?: "operations" | "security";
                /** @description 정확히 일치하는 action (공백 없는 출력 가능 문자 최대 128 byte) */
                action?: string;
                /** @description page 크기 (기본 100, 최대 1,000) */
                limit?: components["parameters"]["Limit"];
                /** @description 앞 응답의 `next_cursor`(서명, 15분). 같은 조건·같은 권한에서만 쓸 수 있다. */
                cursor?: components["parameters"]["Cursor"];
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description 감사 이벤트 page */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["AuditEvent"][];
                        next_cursor: components["schemas"]["NextCursor"];
                        meta: components["schemas"]["ControlMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    validateMonitor: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["MonitorSpecInput"];
            };
        };
        responses: {
            /** @description 정규형 spec, 경고, dry-run 결과 */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: {
                            normalized_spec: components["schemas"]["MonitorSpec"];
                            /** @description for_seconds_zero, minimum_requests_zero, evaluation_below_rollup_resolution, dry_run_* 등 */
                            warnings: string[];
                            /** @description 하지 못했으면 null (warnings에 이유). "발화 없음"과 구별한다 (계약 6) */
                            dry_run: components["schemas"]["MonitorDryRun"] | null;
                        };
                        meta: components["schemas"]["ControlMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            413: components["responses"]["PayloadTooLarge"];
            422: components["responses"]["Unprocessable"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    listMonitors: {
        parameters: {
            query?: {
                /** @description page 크기 (기본 100, 최대 1,000, D02 §12) */
                limit?: number;
                /** @description 앞 응답의 `next_cursor`(서명, 15분). 같은 조건·같은 권한에서만 쓸 수 있다. */
                cursor?: components["parameters"]["Cursor"];
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description monitor page */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["Monitor"][];
                        next_cursor: components["schemas"]["NextCursor"];
                        meta: components["schemas"]["ControlMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    createMonitor: {
        parameters: {
            query?: never;
            header: {
                /** @description 쓰기 POST의 멱등 key(24시간). 같은 key에 다른 본문은 409다 (D02 §20). */
                "Idempotency-Key": components["parameters"]["IdempotencyKey"];
            };
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["MonitorSpecInput"];
            };
        };
        responses: {
            /** @description 만든 monitor (revision 1) */
            201: {
                headers: {
                    ETag: components["headers"]["ETag"];
                    Location: components["headers"]["Location"];
                    "Idempotent-Replayed": components["headers"]["IdempotentReplayed"];
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["Monitor"];
                        meta: components["schemas"]["ControlMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            413: components["responses"]["PayloadTooLarge"];
            422: components["responses"]["Unprocessable"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    getMonitor: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description monitor id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description monitor */
            200: {
                headers: {
                    ETag: components["headers"]["ETag"];
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["Monitor"];
                        meta: components["schemas"]["ControlMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    updateMonitor: {
        parameters: {
            query?: never;
            header: {
                /** @description 읽은 revision의 ETag. 없으면 428, 다르면 412다 (D02 §20). */
                "If-Match": components["parameters"]["IfMatch"];
            };
            path: {
                /** @description monitor id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["MonitorSpecInput"];
            };
        };
        responses: {
            /** @description 새 revision */
            200: {
                headers: {
                    ETag: components["headers"]["ETag"];
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["Monitor"];
                        meta: components["schemas"]["ControlMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            412: components["responses"]["PreconditionFailed"];
            413: components["responses"]["PayloadTooLarge"];
            422: components["responses"]["Unprocessable"];
            428: components["responses"]["PreconditionRequired"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    deleteMonitor: {
        parameters: {
            query?: never;
            header?: {
                /** @description 읽은 revision의 ETag (선택) */
                "If-Match"?: string;
            };
            path: {
                /** @description monitor id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description 삭제됨 */
            204: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            412: components["responses"]["PreconditionFailed"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    listMonitorGroups: {
        parameters: {
            query?: {
                /** @description 최대 group 수 (기본 200, 최대 1,000) */
                limit?: number;
            };
            header?: never;
            path: {
                /** @description monitor id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description group 상태 */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["MonitorGroup"][];
                        meta: components["schemas"]["MonitorGroupsMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    getMonitorTimeline: {
        parameters: {
            query?: {
                /** @description 구간 시작(포함), RFC3339. 기본 to − 24시간 */
                from?: string;
                /** @description 구간 끝(제외), RFC3339. 기본 지금 */
                to?: string;
            };
            header?: never;
            path: {
                /** @description monitor id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description timeline */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["MonitorTimeline"];
                        meta: components["schemas"]["ControlMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    listNotificationPolicies: {
        parameters: {
            query?: {
                /** @description page 크기 (기본 100, 최대 1,000, D02 §12) */
                limit?: number;
                /** @description 앞 응답의 `next_cursor`(서명, 15분). 같은 조건·같은 권한에서만 쓸 수 있다. */
                cursor?: components["parameters"]["Cursor"];
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description policy page */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["NotificationPolicy"][];
                        next_cursor: components["schemas"]["NextCursor"];
                        meta: components["schemas"]["ControlMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    createNotificationPolicy: {
        parameters: {
            query?: never;
            header: {
                /** @description 쓰기 POST의 멱등 key(24시간). 같은 key에 다른 본문은 409다 (D02 §20). */
                "Idempotency-Key": components["parameters"]["IdempotencyKey"];
            };
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["NotificationPolicyCreate"];
            };
        };
        responses: {
            /** @description 만든 policy */
            201: {
                headers: {
                    ETag: components["headers"]["ETag"];
                    Location: components["headers"]["Location"];
                    "Idempotent-Replayed": components["headers"]["IdempotentReplayed"];
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationPolicyResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            413: components["responses"]["PayloadTooLarge"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    getNotificationPolicy: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description policy id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description policy */
            200: {
                headers: {
                    ETag: components["headers"]["ETag"];
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationPolicyResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    updateNotificationPolicy: {
        parameters: {
            query?: never;
            header: {
                /** @description 읽은 revision의 ETag. 없으면 428, 다르면 412다 (D02 §20). */
                "If-Match": components["parameters"]["IfMatch"];
            };
            path: {
                /** @description policy id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["NotificationPolicyUpdate"];
            };
        };
        responses: {
            /** @description 새 revision */
            200: {
                headers: {
                    ETag: components["headers"]["ETag"];
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationPolicyResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            412: components["responses"]["PreconditionFailed"];
            413: components["responses"]["PayloadTooLarge"];
            428: components["responses"]["PreconditionRequired"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    deleteNotificationPolicy: {
        parameters: {
            query?: never;
            header?: {
                /** @description 읽은 revision의 ETag (선택) */
                "If-Match"?: string;
            };
            path: {
                /** @description policy id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description 삭제됨 */
            204: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            412: components["responses"]["PreconditionFailed"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    rotateNotificationPolicySecret: {
        parameters: {
            query?: never;
            header: {
                /** @description 쓰기 POST의 멱등 key(24시간). 같은 key에 다른 본문은 409다 (D02 §20). */
                "Idempotency-Key": components["parameters"]["IdempotencyKey"];
                /** @description 읽은 revision의 ETag. 없으면 428, 다르면 412다 (D02 §20). */
                "If-Match": components["parameters"]["IfMatch"];
            };
            path: {
                /** @description policy id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        requestBody?: {
            content: {
                "application/json": components["schemas"]["RotateSecretRequest"];
            };
        };
        responses: {
            /** @description 새 secret과 새 revision (재생이면 secret 없음) */
            200: {
                headers: {
                    ETag: components["headers"]["ETag"];
                    "Idempotent-Replayed": components["headers"]["IdempotentReplayed"];
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationPolicyResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            412: components["responses"]["PreconditionFailed"];
            413: components["responses"]["PayloadTooLarge"];
            428: components["responses"]["PreconditionRequired"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    sendNotificationPolicyTest: {
        parameters: {
            query?: never;
            header: {
                /** @description 쓰기 POST의 멱등 key(24시간). 같은 key에 다른 본문은 409다 (D02 §20). */
                "Idempotency-Key": components["parameters"]["IdempotencyKey"];
            };
            path: {
                /** @description policy id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description 시험 전송이 쌓였다 (또는 진행 중인 것) */
            202: {
                headers: {
                    Location: components["headers"]["Location"];
                    "Idempotent-Replayed": components["headers"]["IdempotentReplayed"];
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["NotificationTest"];
                        meta: components["schemas"]["ControlMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    getNotificationPolicyTest: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description policy id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
                /** @description 시험 전송 id (소문자 UUID) */
                test_id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description 시험 전송 상태 */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["NotificationTest"];
                        meta: components["schemas"]["ControlMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    listDashboards: {
        parameters: {
            query?: {
                /** @description page 크기 (기본 100, 최대 1,000, D02 §12) */
                limit?: number;
                /** @description 앞 응답의 `next_cursor`(서명, 15분). 같은 조건·같은 권한에서만 쓸 수 있다. */
                cursor?: components["parameters"]["Cursor"];
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description dashboard page */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["DashboardSummary"][];
                        next_cursor: components["schemas"]["NextCursor"];
                        meta: components["schemas"]["ControlMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    createDashboard: {
        parameters: {
            query?: never;
            header: {
                /** @description 쓰기 POST의 멱등 key(24시간). 같은 key에 다른 본문은 409다 (D02 §20). */
                "Idempotency-Key": components["parameters"]["IdempotencyKey"];
            };
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DashboardSpecInput"];
            };
        };
        responses: {
            /** @description 만든 dashboard (revision 1) */
            201: {
                headers: {
                    ETag: components["headers"]["ETag"];
                    Location: components["headers"]["Location"];
                    "Idempotent-Replayed": components["headers"]["IdempotentReplayed"];
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["Dashboard"];
                        meta: components["schemas"]["ControlMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            413: components["responses"]["PayloadTooLarge"];
            422: components["responses"]["Unprocessable"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    getDashboard: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description dashboard id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description dashboard */
            200: {
                headers: {
                    ETag: components["headers"]["ETag"];
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["Dashboard"];
                        meta: components["schemas"]["ControlMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    updateDashboard: {
        parameters: {
            query?: never;
            header: {
                /** @description 읽은 revision의 ETag. 없으면 428, 다르면 412다 (D02 §20). */
                "If-Match": components["parameters"]["IfMatch"];
            };
            path: {
                /** @description dashboard id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DashboardSpecInput"];
            };
        };
        responses: {
            /** @description 새 revision */
            200: {
                headers: {
                    ETag: components["headers"]["ETag"];
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        data: components["schemas"]["Dashboard"];
                        meta: components["schemas"]["ControlMeta"];
                    };
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            412: components["responses"]["PreconditionFailed"];
            413: components["responses"]["PayloadTooLarge"];
            422: components["responses"]["Unprocessable"];
            428: components["responses"]["PreconditionRequired"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
    deleteDashboard: {
        parameters: {
            query?: never;
            header?: {
                /** @description 읽은 revision의 ETag (선택) */
                "If-Match"?: string;
            };
            path: {
                /** @description dashboard id (소문자 UUID) */
                id: components["schemas"]["LowercaseUUID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description 삭제됨 */
            204: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            412: components["responses"]["PreconditionFailed"];
            500: components["responses"]["Internal"];
            503: components["responses"]["Unavailable"];
        };
    };
}
