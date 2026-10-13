// notification policy 화면의 순수 함수 (ADR 0053·0055·0056).
import type { TestItem } from '../../api/notificationPolicies.ts';

/** 전송 오류 code(internal/notify ErrCode*, 저장소 사유) → 사람이 읽는 말. 모르는 code는 그대로 보인다. */
export const DELIVERY_ERROR_TEXT: Record<string, string> = {
  http_status: '2xx가 아닌 응답',
  connect_failed: '연결 실패',
  timeout: '응답 시간 초과(10초)',
  tls_failed: 'TLS 실패(인증서 확인)',
  redirect: 'redirect 응답(따라가지 않습니다)',
  destination_blocked: '사설·내부 주소로 풀려 막았습니다',
  invalid_destination: '주소 형식 오류',
  request_error: '전송 오류',
  policy_disabled: 'policy가 지워져 보내지 않았습니다',
  secret_unavailable: '서명 secret을 풀 수 없습니다(운영팀 문의, RB04)',
};

export const errorText = (code: string | null): string => (code === null ? '' : (DELIVERY_ERROR_TEXT[code] ?? code));

/** 시험 결과 한 줄. 진행 중을 성공으로, 실패를 진행 중으로 보이지 않는다. */
export function testResultText(t: TestItem): { tone: 'success' | 'critical' | 'neutral' | 'info'; text: string } {
  const code = t.last_status_code === null ? '' : ` · 응답 ${String(t.last_status_code)}`;
  switch (t.status) {
    case 'delivered':
      return { tone: 'success', text: `전달됨${code}` };
    case 'failed':
      return { tone: 'critical', text: `실패: ${errorText(t.last_error) || '알 수 없음'}${code}` };
    case 'canceled':
      return { tone: 'neutral', text: `보내지 않음: ${errorText(t.last_error) || '취소'}` };
    default:
      return { tone: 'info', text: '보내는 중…' };
  }
}

/** 이름·주소 입력 검사(서버가 최종 검증한다). 주소는 생성 때 필수, 수정 때 비우면 그대로 둔다. */
export function validatePolicyInput(name: string, url: string, requireURL: boolean): { name?: string; url?: string } {
  const out: { name?: string; url?: string } = {};
  const n = name.trim();
  if (n === '' || [...n].length > 200) out.name = '이름을 1~200자로 입력하세요';
  const u = url.trim();
  if (u === '') {
    if (requireURL) out.url = 'webhook 주소를 입력하세요';
  } else if (!u.startsWith('https://')) {
    out.url = 'https:// 주소만 받습니다';
  }
  return out;
}
