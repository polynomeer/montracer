---
name: adr
description: Montracer ADR(Architecture Decision Record)을 작성하거나 상태를 갱신한다. 기술 선택, 범위·수치 변경, 명세 간 불일치 해소, D06의 ADR 001~012 확정이 필요할 때 사용한다.
---

# ADR 작성

1. `docs/adr/README.md` 등록부를 읽어 번호와 기존 결정을 확인한다.
   - D06 §08~09에 이미 정의된 001~012를 확정하는 경우 그 번호를 그대로 쓴다.
   - 새 결정은 등록부의 가장 큰 번호 + 1.
2. 원문 근거를 읽는다: D06 §08~09와 결정이 닿는 명세 절 (`grep -n "^## " docs/specs/Dxx-*.md`로 찾기).
3. `docs/adr/template.md`를 복사해 `docs/adr/NNNN-kebab-title.md`로 작성한다.
   - 배경·결정·후보·결과·Rollback·재검토 조건·증거를 모두 채운다.
   - 수치는 "목표" 또는 "가정"임을 밝히고 출처 절을 단다.
4. 상태 규칙
   - 사용자가 승인자·날짜를 명시하지 않았다면 상태는 `제안`이다. 승인 이력을 지어내지 않는다.
   - 기존 ADR을 바꾸는 경우 삭제하지 않고 옛 ADR을 `대체됨(superseded by NNNN)`으로 표시한다.
5. `docs/adr/README.md` 등록부의 해당 행(상태, 원문 링크)을 갱신한다.
6. 결정이 D01~D06 내용과 다르면, 어떤 권위 문서의 어느 절을 개정해야 하는지와 영향 받는 계약 테스트를 ADR "결과"에 적고 사용자에게 알린다.
7. 커밋: `docs(adr): ADR NNNN <제목> <상태>`.

$ARGUMENTS
