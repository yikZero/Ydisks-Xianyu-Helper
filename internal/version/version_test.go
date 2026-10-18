package version

import "testing"

func TestShortCommit(t *testing.T) {
	original := Commit
	t.Cleanup(func() { Commit = original })

	Commit = "0123456789abcdef"
	if got := ShortCommit(); got != "0123456789ab" {
		t.Fatalf("ShortCommit() = %q", got)
	}
	Commit = "   "
	// got 是空提交标识的稳定回退值。
	if got := ShortCommit(); got != "unknown" {
		t.Fatalf("空提交标识回退值=%q", got)
	}
	Commit = "abc123"
	// got 是短提交标识的原样返回值。
	if got := ShortCommit(); got != "abc123" {
		t.Fatalf("短提交标识=%q", got)
	}
}
