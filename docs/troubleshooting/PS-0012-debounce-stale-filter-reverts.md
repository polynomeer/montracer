# PS-0012: 조건 입력 debounce가 기다리는 사이 바꾼 다른 조건(심각도 등)을 되돌림

- 날짜: 2026-10-13 (발견) / 2026-10-13 (해결)
- 영역: web (apps/web) — S04 Trace Explorer, S08 Logs·Metrics 조건 입력
- 영향: 사용성 — 검색어를 입력하고 300ms 안에 심각도·서비스 등을 바꾸면 그 변경이 말없이 사라진다. 시험이 부하 상황에서 가끔 실패한다(flaky). 데이터·tenant 영향 없음. 심각도 P2
- 수정: 브랜치 `fix/web-debounce-stale-filter`, `apps/web/src/app/useDebouncedCommit.ts`
- 관련: [PS-0008](PS-0008-debounced-input-reverted.md)(같은 debounce 경로의 다른 결함), ADR 0043·0045·0047(입력은 300ms 뒤 또는 Enter에 반영, D05 §03), ADR 0041(URL이 조사 상태의 원천)

## 증상

전체 web 시험(`pnpm -s test`)에서 `LogExplorer.test.tsx`의 "조건 입력: 잘못된 trace ID는 반영하지 않고, 조회 버튼은 바로 반영"이 여러 번에 한 번 실패했다. 실패 메시지는 `expected null to be 'warn'`이다. 최소 심각도를 `warn`으로 고른 직후 URL의 `sev`가 비어 있었다. 단독 실행(`vitest run src/features/logs`)과 재실행에서는 통과했다. 실패한 회차의 실행 시간은 약 775ms로, 평소보다 길었다.

## 발견 경위

다른 작업 중 전체 시험을 돌리다 두 번 관측했다. 수정 전 코드로 전체 시험을 6회 돌려 1회 재현했다. 시험은 `Date`만 가짜로 돌리고 `setTimeout`은 실제 시간을 쓴다. 그래서 debounce 발화 시점이 부하에 따라 달랐고, 단독 실행에서는 거의 드러나지 않았다.

## 원인

조건 입력은 입력값이 바뀔 때마다 `useEffect`로 300ms 타이머를 예약했다.

```ts
useEffect(() => {
  if (query === filter.query && trace === (filter.traceId ?? '')) return;
  const t = window.setTimeout(() => commit(query, trace), 300);
  return () => window.clearTimeout(t);
}, [query, trace]);
```

1. 타이머가 부르는 `commit`은 **예약한 render**의 함수다. 그 closure의 `filter`와 상위 `setFilter`의 `params`도 예약 시점 값이다.
2. effect는 입력값(`query`, `trace`)이 바뀔 때만 다시 돈다. 심각도 같은 다른 조건이 바뀌어도 예약은 그대로 남는다.
3. 그래서 마지막 글자 뒤 300ms 안에 심각도를 바꾸면, 타이머가 `onChange({ ...예약 시점 filter, query })`를 부른다. 그 결과 URL이 `sev` 없는 값으로 덮인다.
4. 조회 버튼·Enter는 `commit`을 바로 불렀지만 남은 타이머를 지우지 않았다. 타이머가 늦게 같은 값을 한 번 더 반영했고, history 항목이 하나 더 생겼다.

시험에서는 마지막 글자 입력, 조회 클릭, 심각도 선택이 보통 300ms 안에 끝난다. 이때 타이머는 assertion 뒤에 발화하므로 시험이 통과한다. 부하가 걸려 userEvent 단계 사이가 길어지면 타이머가 `selectOptions`와 assertion 사이에 끼어든다. 이 경우 `sev`가 되돌려져 시험이 실패한다. S04 Trace(span 이름·최소 duration)와 S08 Metrics(조건 행)도 같은 패턴이었다.

## 해결

`apps/web/src/app/useDebouncedCommit.ts`를 추가해 세 화면이 함께 쓰게 했다.

- 타이머는 발화 시점의 **최신** `commit`을 ref로 부른다. 최신 `commit`은 최신 `filter`와 `params`를 가진다. 이 ref 패턴은 `useRemote`의 `fetcherRef`와 같다.
- 예약은 여전히 입력값(`draftKey`)이 바뀔 때만 다시 건다. 다른 조건이 바뀌어도 300ms를 새로 세지 않는다.
- 반환하는 `flush()`는 남은 예약을 지우고 바로 반영한다. 조회 버튼과 Enter가 이를 쓴다.

버린 대안은 다음과 같다.

- **시험에서 `setTimeout`을 가짜로만 바꾸기.** flaky는 사라진다. 하지만 실제 사용자에게 같은 결함(바꾼 심각도가 사라짐)이 남는다.
- **다른 조건이 바뀔 때도 effect를 다시 돌리기(deps에 `filter` 추가).** 심각도를 바꿀 때마다 입력 반영의 300ms가 다시 시작된다. 또 closure 문제를 deps 관리로 우회할 뿐이라, 조건이 늘면 같은 실수가 다시 생긴다.
- **상위 `setFilter`를 `setSearchParams(prev => …)` 함수형으로 바꾸기.** `params`는 최신이 된다. 하지만 패널의 `filter` closure가 여전히 예약 시점 값이라 `{ ...filter }`가 다른 조건을 되돌린다.

## 재발 방지

- 시험: `apps/web/src/features/logs/LogExplorer.test.tsx`
  - "조건 입력: 본문 입력 뒤 300ms 안에 바꾼 심각도를 debounce 반영이 되돌리지 않는다" — 가짜 `setTimeout`으로 순서를 고정한다. 수정 전 코드에서는 늘 `expected null to be 'warn'`으로 실패한다.
  - "조건 입력: 조회 버튼으로 반영하면 남은 debounce가 늦게 다시 반영하지 않는다" — 조회 뒤 1초 동안 location key가 바뀌지 않는지 본다. 수정 전 코드에서는 늘 실패한다.
- 시험: `apps/web/src/app/useDebouncedCommit.test.ts` — 최신 commit 호출, `pending=false`이면 예약 안 함, `flush`의 예약 취소, unmount 시 취소를 고정한다. S04·Metrics도 같은 hook을 쓰므로 이 시험이 함께 지킨다.
- 검증: 수정 뒤 전체 web 시험을 10회 연속 돌려 모두 통과했다(211개).

## 교훈

- `setTimeout`·`useEffect` 안에서 render closure의 상태로 "전체 상태 = {...상태, 바뀐 값}"을 만들면, 기다리는 사이 바뀐 다른 값을 되돌린다. 늦게 실행되는 callback은 최신 값을 ref로 읽는다.
- 실제 시간 타이머에 의존하는 UI 시험은 부하에 따라 실행 순서가 바뀐다. 순서가 중요한 시험은 가짜 타이머로 순서를 고정한다. flaky 시험은 먼저 제품 결함인지 확인한다(PS-0008도 같은 경로에서 flaky로 드러난 실제 결함이었다).
