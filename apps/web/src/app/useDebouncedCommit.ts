import { useEffect, useRef } from 'react';

/**
 * 입력 초안을 `delayMs` 뒤 URL에 반영하는 예약 (D05 §03: 입력은 300ms 뒤 또는 Enter에 반영).
 *
 * 예약은 `draftKey`(입력값)가 바뀔 때만 다시 건다. 타이머가 부르는 것은 예약한 render의 `commit`이 아니라
 * **발화 시점의 최신 `commit`**이다. 예약한 render의 것을 부르면 그 render의 조건·URL로 반영해서,
 * 기다리는 사이 바꾼 다른 조건(심각도·서비스 등)을 되돌린다(PS-0012).
 *
 * - `pending`: 입력이 URL과 다를 때만 true. false면 예약하지 않는다.
 * - 반환값 `flush()`: 남은 예약을 지우고 바로 반영한다(Enter·조회 버튼). 같은 값을 늦게 한 번 더 반영하지 않는다.
 */
export function useDebouncedCommit(draftKey: string, pending: boolean, commit: () => void, delayMs = 300) {
  const latest = useRef(commit);
  latest.current = commit;
  const timer = useRef<number | null>(null);
  const cancel = () => {
    if (timer.current === null) return;
    window.clearTimeout(timer.current);
    timer.current = null;
  };
  useEffect(() => {
    if (!pending) return;
    timer.current = window.setTimeout(() => {
      timer.current = null;
      latest.current();
    }, delayMs);
    return cancel;
  }, [draftKey]);
  return () => {
    cancel();
    latest.current();
  };
}
