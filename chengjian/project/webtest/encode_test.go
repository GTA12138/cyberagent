package webtest

import (
	"strings"
	"testing"
)

func TestEncodeKinds(t *testing.T) {
	cases := []struct {
		kind, in, want string
	}{
		{"", "a b'", "a b'"},                                   // 不编码
		{"url", "a b'", "a+b%27"},                              // 空格→+，'→%27
		{"url2", "'", "%2527"},                                 // 双层：%27 再编成 %2527
		{"base64", "admin", "YWRtaW4="},
		{"hex", "AB", "4142"},
		{"unicode", "A", "%u0041"},
		{"unicode", "<>", "%u003C%u003E"},
	}
	for _, c := range cases {
		got, err := Encode(c.kind, c.in)
		if err != nil {
			t.Fatalf("Encode(%q,%q) 出错: %v", c.kind, c.in, err)
		}
		if got != c.want {
			t.Errorf("Encode(%q,%q)=%q, want %q", c.kind, c.in, got, c.want)
		}
	}
	if _, err := Encode("rot13", "x"); err == nil || !strings.Contains(err.Error(), "未知编码类别") {
		t.Fatalf("未知类别必须报错（防「以为编了码其实没编」），got %v", err)
	}
}
