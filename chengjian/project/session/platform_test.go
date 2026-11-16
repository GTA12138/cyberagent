package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// platSession 是可读 Kind、可选实现 HostPlatformProvider 的测试会话。
// exec 返回值由用例指定，用于驱动方言探测。
type platSession struct {
	kind   string
	os     string
	exec   string
	execEr error
}

func (s *platSession) ID() int64                  { return 1 }
func (s *platSession) Kind() string               { return s.kind }
func (s *platSession) Test(context.Context) error { return nil }
func (s *platSession) Exec(context.Context, string, time.Duration) (string, string, error) {
	return s.exec, "", s.execEr
}
func (s *platSession) ReadFile(context.Context, string) ([]byte, error) { return nil, nil }
func (s *platSession) WriteFile(context.Context, string, []byte) error  { return nil }
func (s *platSession) Close(context.Context) error                      { return nil }

type platProvider struct{ platSession }

func (p *platProvider) HostPlatform() string { return p.os }

// 驱动没自报时必须是 ""（未知），绝不能默认 linux：把「不知道」降级成
// 「按 Unix 办」正是这条链路上一连串误判的起点。
func TestDescribePlatformUnknownIsEmpty(t *testing.T) {
	cases := map[string]Session{
		"nil":            nil,
		"no_provider":    &platSession{kind: "http_aspx"},
		"empty_value":    &platProvider{platSession{os: ""}},
		"unrecognized":   &platProvider{platSession{os: "solaris"}},
		"typed_nil_http": (*HTTPShell)(nil),
	}
	for name, s := range cases {
		if got := DescribePlatform(s); got != "" {
			t.Errorf("%s: DescribePlatform = %q, want \"\"", name, got)
		}
	}
}

func TestDescribePlatformNormalizesKnownValues(t *testing.T) {
	cases := map[string]string{
		"windows": PlatformWindows,
		"WIN64":   PlatformWindows,
		" Win ":   PlatformWindows,
		"linux":   PlatformLinux,
		"unix":    PlatformLinux,
	}
	for in, want := range cases {
		if got := DescribePlatform(&platProvider{platSession{os: in}}); got != want {
			t.Errorf("DescribePlatform(%q) = %q, want %q", in, got, want)
		}
	}
}

// 驱动自报优先于马型推断：一个自报 linux 的会话，即使 kind 里有 aspx 字样
// 也不能被当成 Windows（反向也成立）。
func TestPlatformForSessionPrefersDriverFact(t *testing.T) {
	if got := PlatformForSession(&platProvider{platSession{kind: "http_aspx", os: "linux"}}); got != PlatformLinux {
		t.Errorf("驱动自报 linux 应胜出，得到 %q", got)
	}
	if got := PlatformForSession(&platProvider{platSession{kind: "http_php", os: "windows"}}); got != PlatformWindows {
		t.Errorf("驱动自报 windows 应胜出，得到 %q", got)
	}
}

// 没有驱动自报时退回马型推断——这是既有行为，不能被这次重构改掉。
func TestPlatformForSessionFallsBackToKind(t *testing.T) {
	for _, k := range []string{"http_aspx", "http_aspx_enc", "asp", "reverse_windows"} {
		if got := PlatformForSession(&platSession{kind: k}); got != PlatformWindows {
			t.Errorf("kind %q 应推断为 windows，得到 %q", k, got)
		}
	}
	for _, k := range []string{"http_php", "http_jsp", "http_phpcmd", "reverse", ""} {
		if got := PlatformForSession(&platSession{kind: k}); got != PlatformLinux {
			t.Errorf("kind %q 应推断为 linux，得到 %q", k, got)
		}
	}
}

// 方言探测：sh 会把 $((40+2)) 算成 42，cmd 原样输出。判不出来必须是 ""。
func TestDetectShellDialect(t *testing.T) {
	cases := []struct {
		name string
		sess *platSession
		want string
	}{
		{"linux", &platSession{exec: "__A42__"}, PlatformLinux},
		{"windows", &platSession{exec: "__A$((40+2))__"}, PlatformWindows},
		{"silent", &platSession{exec: ""}, ""},
		{"garbage", &platSession{exec: "Access denied"}, ""},
		{"error_with_output", &platSession{exec: "__A42__", execEr: errors.New("boom")}, PlatformLinux},
	}
	for _, c := range cases {
		if got := DetectShellDialect(context.Background(), c.sess, time.Second); got != c.want {
			t.Errorf("%s: DetectShellDialect = %q, want %q", c.name, got, c.want)
		}
	}
	// nil 会话不得 panic。
	if got := DetectShellDialect(context.Background(), nil, time.Second); got != "" {
		t.Errorf("nil 会话应返回 \"\"，得到 %q", got)
	}
}

// 探测命令本身必须同时含算术展开与重定向：少了任何一个，两种解释器的回显
// 就不再可区分（cmd 会把 2>&1 当重定向吃掉，sh 也会）。
func TestDialectProbeShape(t *testing.T) {
	for _, want := range []string{"$((40+2))", "2>&1", "__A"} {
		if !strings.Contains(dialectProbe, want) {
			t.Errorf("方言探测命令缺少 %q：%s", want, dialectProbe)
		}
	}
}
