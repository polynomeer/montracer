package contract

import (
	"fmt"
	"sort"

	"github.com/pb33f/libopenapi"
	"github.com/pb33f/libopenapi/what-changed/model"
)

// Breaking은 base → head에서 호환을 깨는 변경을 돌려준다 (D02 §12 "additive 필드는 v1, 의미 변경은 v2", ADR 0059 §3).
// 판정은 libopenapi what-changed 규칙이다: path·operation·응답 field 삭제, 요청 필수 field 추가, 타입 변경 등.
// 응답 field 추가·선택 요청 field 추가·설명 변경은 깨지 않는다.
func Breaking(base, head []byte) ([]string, error) {
	b, err := libopenapi.NewDocument(base)
	if err != nil {
		return nil, fmt.Errorf("contract: parse base spec: %w", err)
	}
	h, err := libopenapi.NewDocument(head)
	if err != nil {
		return nil, fmt.Errorf("contract: parse head spec: %w", err)
	}
	changes, err := libopenapi.CompareDocuments(b, h)
	if err != nil {
		return nil, fmt.Errorf("contract: compare: %w", err)
	}
	if changes == nil {
		return nil, nil
	}
	var out []string
	for _, c := range changes.GetAllChanges() {
		if !c.Breaking {
			continue
		}
		line := 0
		if c.Context != nil && c.Context.NewLine != nil {
			line = *c.Context.NewLine
		} else if c.Context != nil && c.Context.OriginalLine != nil {
			line = *c.Context.OriginalLine
		}
		out = append(out, fmt.Sprintf("line %d: %s %q: %q → %q", line, changeKind(c.ChangeType), c.Property, c.Original, c.New))
	}
	sort.Strings(out)
	return out, nil
}

func changeKind(t int) string {
	switch t {
	case model.Modified:
		return "modified"
	case model.PropertyAdded:
		return "property added"
	case model.ObjectAdded:
		return "object added"
	case model.ObjectRemoved:
		return "object removed"
	case model.PropertyRemoved:
		return "property removed"
	default:
		return fmt.Sprintf("change(%d)", t)
	}
}
