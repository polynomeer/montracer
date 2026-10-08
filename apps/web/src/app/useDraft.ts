import { useEffect, useRef, useState } from 'react';

/**
 * URL이 원천인 입력의 초안 (D05 §03: 입력은 300ms 뒤 또는 Enter에 반영).
 *
 * 입력 중 debounce로 URL에 반영하면 URL 값이 바뀌고, "URL → 입력" 동기화가 그 사이에 더 친 글자를 지운다(PS-0008).
 * 그래서 **우리가 반영한 값**은 동기화하지 않고, 바깥에서 바뀐 URL(뒤로 가기·링크·다른 조건 변경)만 입력에 다시 넣는다.
 *
 * - `committed(v)`: URL에 쓴 값(정규화 뒤, 예: trim·소문자·빈 행 제거)을 알린다. 그 값으로 돌아오는 URL 변경은 무시한다.
 * - `key`는 값 비교용 문자열이다(배열·객체 값).
 */
export function useDraft<T>(external: T, key: (v: T) => string = (v) => JSON.stringify(v)) {
  const [draft, setDraft] = useState<T>(external);
  const externalKey = key(external);
  const last = useRef(externalKey);
  useEffect(() => {
    if (externalKey === last.current) return;
    last.current = externalKey;
    setDraft(external);
  }, [externalKey]);
  const committed = (v: T) => {
    last.current = key(v);
  };
  return [draft, setDraft, committed] as const;
}
