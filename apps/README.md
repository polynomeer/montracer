# apps

사용자에게 배포되는 애플리케이션.

| 경로 | 내용 | 명세 |
|---|---|---|
| `web/` | React + TypeScript UI. `src/features/`는 기능별 화면(services, traces, inspector, monitors, dashboards, profiles, dbm, rum, settings) | D05 전체, D06 §11 |

규칙 (D05 §04, D06 §11)
- 서버 상태는 tenant·auth fingerprint를 포함한 key로 관리하고 전역 store에 서버 원본을 복사하지 않는다.
- UI 컴포넌트(`packages/ui`)는 domain API를 직접 호출하지 않고 typed props와 event만 쓴다.
- log·stack·SQL은 text-only 렌더링. raw HTML 금지.
- 라우트 가드는 보안 경계가 아니다. 서버 재인가를 전제로 401/403/404/429/503을 처리한다.
