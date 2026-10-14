// dashboard 위젯 조회 묶음 (D05 §10 "같은 query는 합치고 view당 병렬 6개", §11 "50 query를 한 번에 발사하지 않는다").
// - 진행 중인 같은 조회(같은 key)는 한 요청을 같이 기다린다.
// - 동시에 도는 요청은 max개까지이고, 나머지는 순서대로 기다린다.
// - 기다리던 위젯이 모두 떠나면(취소) 요청도 취소한다. 결과는 위젯 상태로만 간다(전역 store에 두지 않는다, D05 §04).
import type { ApiResult } from '../../api/client.ts';

interface Entry<T> {
  promise: Promise<ApiResult<T>>;
  waiting: number;
  ctrl: AbortController;
}

interface Waiter {
  grant: () => void;
  reject: (e: unknown) => void;
}

const abortError = () => new DOMException('aborted', 'AbortError');

export class QueryPool<T> {
  private readonly inflight = new Map<string, Entry<T>>();
  private readonly queue: Waiter[] = [];
  private running = 0;

  constructor(
    private readonly max: number,
    private readonly fetcher: (key: string, signal: AbortSignal) => Promise<ApiResult<T>>,
  ) {}

  /** 지금 도는 요청 수(시험용) */
  get active(): number {
    return this.running;
  }

  run(key: string, signal: AbortSignal): Promise<ApiResult<T>> {
    if (signal.aborted) return Promise.reject(abortError());
    let entry = this.inflight.get(key);
    if (entry === undefined) {
      const ctrl = new AbortController();
      const promise = this.acquire(ctrl.signal)
        .then(async (release) => {
          try {
            return await this.fetcher(key, ctrl.signal);
          } finally {
            release();
          }
        })
        .finally(() => {
          if (this.inflight.get(key)?.ctrl === ctrl) this.inflight.delete(key);
        });
      entry = { promise, waiting: 0, ctrl };
      this.inflight.set(key, entry);
    }
    const e = entry;
    e.waiting++;
    return new Promise<ApiResult<T>>((resolve, reject) => {
      let settled = false;
      const leave = () => {
        if (settled) return;
        settled = true;
        e.waiting--;
        if (e.waiting === 0) {
          e.ctrl.abort(); // 아무도 기다리지 않으면 요청(또는 대기)을 그만둔다
          if (this.inflight.get(key) === e) this.inflight.delete(key);
        }
        reject(abortError());
      };
      signal.addEventListener('abort', leave, { once: true });
      e.promise.then(
        (r) => {
          if (settled) return;
          settled = true;
          signal.removeEventListener('abort', leave);
          resolve(r);
        },
        (err: unknown) => {
          if (settled) return;
          settled = true;
          signal.removeEventListener('abort', leave);
          reject(err instanceof Error || err instanceof DOMException ? err : new Error(String(err)));
        },
      );
    });
  }

  /** 자리를 얻으면 반납 함수를 준다. 기다리는 중에 취소되면 줄에서 빠진다. */
  private acquire(signal: AbortSignal): Promise<() => void> {
    return new Promise((resolve, reject) => {
      const grant = () => {
        this.running++;
        let released = false;
        resolve(() => {
          if (released) return;
          released = true;
          this.running--;
          this.queue.shift()?.grant();
        });
      };
      if (this.running < this.max) {
        grant();
        return;
      }
      const waiter: Waiter = { grant, reject };
      this.queue.push(waiter);
      signal.addEventListener(
        'abort',
        () => {
          const i = this.queue.indexOf(waiter);
          if (i >= 0) {
            this.queue.splice(i, 1);
            reject(abortError());
          }
        },
        { once: true },
      );
    });
  }
}
