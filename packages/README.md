# packages

Frontend 공유 패키지 (D05, D06 §11).

| 경로 | 내용 |
|---|---|
| `design-tokens/` | light/dark 색상·타이포·치수 토큰 (D05 §02) |
| `ui/` | TimeRangePicker, QueryBar, FacetPanel, DataTable, MetricChart, EntityDrawer, StatusBadge, ConfirmDialog — 상태 계약 포함 (D05 §03). domain API 직접 호출 금지 |
| `query-schema/` | query AST·unit·time·role 공유 타입. `api/openapi`에서 생성·검증 |

- 독립 브랜드: 벤치마크 제품의 로고·색상·화면 자산을 복제하지 않는다 (D01 §04, ADR 012).
