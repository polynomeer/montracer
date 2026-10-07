import { Link, useOutletContext, useParams } from 'react-router';
import { formatRange, type InvestigationContext } from './context.ts';
import { orgPath, type Screen } from './nav.ts';

/** 아직 구현하지 않은 화면. 라우트·context 전달을 확인할 수 있게 현재 범위를 보여준다. */
export function ScreenPlaceholder({ screen }: { screen: Screen }) {
  const context = useOutletContext<InvestigationContext>();
  return (
    <section className="mt-placeholder" aria-labelledby="screen-title">
      <p className="mt-label">
        {screen.id ?? '메뉴'} · {screen.spec}
      </p>
      <h1 id="screen-title" className="mt-title">
        {screen.title}
      </h1>
      <p>이 화면은 아직 구현되지 않았습니다.</p>
      <dl className="mt-placeholder__context">
        <dt>환경</dt>
        <dd>{context.environment ?? '전체 환경'}</dd>
        <dt>시간</dt>
        <dd>{formatRange(context.range, Date.now(), context.timeZone)}</dd>
      </dl>
    </section>
  );
}

/** 없는 화면과 볼 권한이 없는 resource를 구별하지 않는다 (D05 §01, §04 404). */
export function NotFound() {
  const { org } = useParams();
  return (
    <section className="mt-placeholder" aria-labelledby="not-found-title">
      <h1 id="not-found-title" className="mt-title">
        페이지를 찾을 수 없습니다
      </h1>
      <p>주소가 바뀌었거나 볼 수 있는 권한이 없습니다.</p>
      {org !== undefined && <Link to={orgPath(org, 'overview')}>Overview로 이동</Link>}
    </section>
  );
}
