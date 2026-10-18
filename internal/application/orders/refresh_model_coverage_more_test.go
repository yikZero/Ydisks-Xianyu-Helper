package orders

import "testing"

// TestSameRefreshOrderTimeNormalizesDatabaseAndPlatformFormats 验证数据库无时区文本与平台 RFC3339 时间不会造成虚假更新。
func TestSameRefreshOrderTimeNormalizesDatabaseAndPlatformFormats(t *testing.T) {
	// cases 覆盖等价时间、不同时间和非法文本，避免统计逻辑依赖字符串外观。
	cases := []struct {
		name  string
		left  string
		right string
		want  bool
	}{
		{name: "utc database text", left: "2026-08-01 12:00:00", right: "2026-08-01T12:00:00Z", want: true},
		{name: "offset text", left: "2026-08-01 20:00:00+08:00", right: "2026-08-01T12:00:00Z", want: true},
		{name: "date text", left: "2026-08-01", right: "2026-08-01T00:00:00Z", want: true},
		{name: "different", left: "2026-08-01 12:00:00", right: "2026-08-01T12:00:01Z", want: false},
		{name: "invalid", left: "not-a-time", right: "2026-08-01T12:00:00Z", want: false},
	}
	for /* tc 表示当前时间格式归一化测试场景。 */ _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// got 表示当前两种时间文本是否代表同一时刻。
			if got := sameRefreshOrderTime(tc.left, tc.right); got != tc.want {
				t.Fatalf("sameRefreshOrderTime(%q,%q)=%v want %v", tc.left, tc.right, got, tc.want)
			}
		})
	}
}
