package quota

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
)

// overridesFile은 overrides 파일 형식이다.
//
//	{"tenants": {"<tenant UUID>": {"traces": {"records_per_second": 50000, "records_burst": 500000,
//	                                         "bytes_per_second": 50000000, "bytes_burst": 60000000}}}}
//
// 네 값은 모두 있어야 한다(일부만 바꾸는 merge는 실수로 0이 들어가는 것을 막기 위해 지원하지 않는다).
type overridesFile struct {
	Tenants Overrides `json:"tenants"`
}

var signals = map[string]bool{"traces": true, "logs": true, "metrics": true}

// ParseOverrides는 파일 내용을 검증해 읽는다. 알 수 없는 필드·tenant 형식 오류·0 이하 값은 거절한다.
func ParseOverrides(b []byte) (Overrides, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f overridesFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("quota: overrides: %w", err)
	}
	for tenant, bySignal := range f.Tenants {
		t, err := authz.ParseTenantID(tenant)
		if err != nil || t.String() != tenant {
			return nil, fmt.Errorf("quota: overrides: tenant %q must be a lowercase UUID", tenant)
		}
		for sig, l := range bySignal {
			if !signals[sig] {
				return nil, fmt.Errorf("quota: overrides: tenant %s: unknown signal %q", tenant, sig)
			}
			if !l.valid() {
				return nil, fmt.Errorf("quota: overrides: tenant %s %s: all four limits must be > 0", tenant, sig)
			}
			if l.ActiveSeries < 0 || (l.ActiveSeries > 0 && sig != "metrics") {
				return nil, fmt.Errorf("quota: overrides: tenant %s %s: active_series is a positive metrics-only limit", tenant, sig)
			}
		}
	}
	if f.Tenants == nil {
		f.Tenants = Overrides{}
	}
	return f.Tenants, nil
}

// FileOverrides는 overrides 파일을 주기적으로 다시 읽는다 (Mimir runtime config와 같은 방식).
// 잘못된 파일은 적용하지 않고 직전 값을 유지한다 — 잘못된 편집 하나로 모든 tenant 한도가 풀리거나 막히지 않게 한다.
type FileOverrides struct {
	path    string
	current atomic.Pointer[Overrides]
	sum     [sha256.Size]byte // 마지막으로 적용한 내용의 해시 (mtime·크기만으로는 같은 초 안의 편집을 놓친다)
}

// LoadFileOverrides는 파일을 처음 읽는다. 기동 시 파일이 잘못됐으면 오류다(조용히 기본값으로 돌지 않는다).
func LoadFileOverrides(path string) (*FileOverrides, error) {
	f := &FileOverrides{path: path}
	if _, err := f.reload(); err != nil {
		return nil, err
	}
	return f, nil
}

// Get은 현재 overrides다. Config.Overrides에 넘긴다.
func (f *FileOverrides) Get() Overrides { return *f.current.Load() }

// reload는 파일이 바뀌었으면 다시 읽는다. 바뀌었는지와 오류를 돌려준다.
func (f *FileOverrides) reload() (bool, error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return false, fmt.Errorf("quota: overrides: %w", err)
	}
	sum := sha256.Sum256(b)
	if f.current.Load() != nil && sum == f.sum {
		return false, nil
	}
	o, err := ParseOverrides(b)
	if err != nil {
		return false, err
	}
	f.current.Store(&o)
	f.sum = sum
	return true, nil
}

// Watch는 ctx가 끝날 때까지 interval마다 파일을 확인한다. 반영되면 onReload, 실패하면 onError를 부른다.
func (f *FileOverrides) Watch(ctx context.Context, interval time.Duration, onReload func(), onError func(error)) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			changed, err := f.reload()
			switch {
			case err != nil && onError != nil:
				onError(err)
			case changed && onReload != nil:
				onReload()
			}
		}
	}
}
