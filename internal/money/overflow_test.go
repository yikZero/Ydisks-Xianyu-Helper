package money

import (
	"errors"
	"testing"
)

// TestParseYuanToCentsRejectsInt64Overflow 验证整数元超出 int64 或乘以一百后溢出时一律拒绝。
func TestParseYuanToCentsRejectsInt64Overflow(t *testing.T) {
	// raw 是依次尝试的超大金额文本：刚好超过可换算上限，以及超过 int64 本身。
	for _, raw := range []string{"92233720368547759", "99999999999999999999.99"} {
		if _, err := ParseYuanToCents(raw); !errors.Is(err, ErrInvalidYuan) {
			t.Fatalf("raw=%s err=%v", raw, err)
		}
	}
}
