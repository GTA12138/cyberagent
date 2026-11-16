package db

import (
	"strings"
	"testing"
)

const sampleBody = `HTTP/1.1 200 OK
Content-Type: text/html

<html><body>
<h1>Welcome admin</h1>
<script>alert('XSS')</script>
<p>token: eyJhbGciOiJIUzI1NiJ9.secret</p>
</body></html>`

// 锚定必须真的定位到响应里的字节区间，并给出可验证的 hash。
func TestAnchorSliceFindsSnippet(t *testing.T) {
	ref := TrafficRef{TrafficID: "t1", Role: "proof"}
	got, err := AnchorSlice(ref, []byte(sampleBody), "alert('XSS')")
	if err != nil {
		t.Fatalf("应能锚定: %v", err)
	}
	if !got.Anchored() {
		t.Fatal("应标记为已锚定")
	}
	if got.SliceHash == "" {
		t.Fatal("应生成 slice_hash")
	}
	if SliceText(got, []byte(sampleBody)) != "alert('XSS')" {
		t.Fatalf("切片文本不对: %q", SliceText(got, []byte(sampleBody)))
	}
}

// 关键用例：模型声称的证据不在响应里时必须报错，而不是静默存一条空锚点。
// 这正是「回执锚定」要拦的那类污染（模型复述而非真实字节）。
func TestAnchorSliceRejectsFabricatedSnippet(t *testing.T) {
	ref := TrafficRef{TrafficID: "t1", Role: "proof"}
	_, err := AnchorSlice(ref, []byte(sampleBody), "root:x:0:0:root:/root:/bin/bash")
	if err == nil {
		t.Fatal("证据不在响应里时必须报错")
	}
	if !strings.Contains(err.Error(), "未在响应体中找到") {
		t.Fatalf("错误信息应点明「没找到」: %v", err)
	}
}

func TestAnchorSliceRejectsEmptySnippet(t *testing.T) {
	if _, err := AnchorSlice(TrafficRef{TrafficID: "t"}, []byte(sampleBody), "   "); err == nil {
		t.Fatal("空片段应报错")
	}
}

func TestAnchorSliceRejectsOversizedSlice(t *testing.T) {
	big := strings.Repeat("A", MaxEvidenceSliceBytes+100)
	if _, err := AnchorSlice(TrafficRef{TrafficID: "t"}, []byte(big), big); err == nil {
		t.Fatal("超过上限的切片应报错")
	}
}

// 内容未变 → 验证通过；内容变了 → 必须失败（否则锚定没有意义）。
func TestVerifySliceDetectsChange(t *testing.T) {
	ref, err := AnchorSlice(TrafficRef{TrafficID: "t1", Role: "proof"}, []byte(sampleBody), "alert('XSS')")
	if err != nil {
		t.Fatal(err)
	}
	ok, verr := VerifySlice(ref, []byte(sampleBody))
	if !ok || verr != nil {
		t.Fatalf("内容未变应通过: ok=%v err=%v", ok, verr)
	}

	// 把证据那段改掉 → hash 不匹配。
	tampered := strings.Replace(sampleBody, "alert('XSS')", "alert('SAFE')", 1)
	ok, verr = VerifySlice(ref, []byte(tampered))
	if ok || verr == nil {
		t.Fatal("内容变化后必须验证失败")
	}
	if !strings.Contains(verr.Error(), "hash 不匹配") {
		t.Fatalf("错误信息应点明 hash 不匹配: %v", verr)
	}

	// 响应被截断 → 切片越界，也应失败。
	truncated := sampleBody[:20]
	ok, verr = VerifySlice(ref, []byte(truncated))
	if ok || verr == nil {
		t.Fatal("响应截断后必须验证失败")
	}
}

// 未锚定的引用「无法验证」，但这不等于「验证失败」——两者必须区分，
// 否则所有历史证据都会被判成坏证据。
func TestVerifySliceUnanchoredIsNotFailure(t *testing.T) {
	ok, err := VerifySlice(TrafficRef{TrafficID: "t1", Role: "supporting"}, []byte(sampleBody))
	if ok || err != nil {
		t.Fatalf("未锚定应返回 (false,nil)，实际 ok=%v err=%v", ok, err)
	}
}

// 形状校验：锚定了就必须有 hash、区间合法、不超上限；未锚定一律通过。
func TestValidateSliceRefs(t *testing.T) {
	cases := []struct {
		name    string
		ref     TrafficRef
		wantErr bool
	}{
		{"完全未锚定（零值）", TrafficRef{TrafficID: "t1"}, false},
		{"合法锚定", TrafficRef{TrafficID: "t1", SliceStart: 10, SliceEnd: 20, SliceHash: "abc"}, false},
		{"锚定但缺 hash", TrafficRef{TrafficID: "t1", SliceStart: 10, SliceEnd: 20}, true},
		{"区间反向", TrafficRef{TrafficID: "t1", SliceStart: 20, SliceEnd: 10, SliceHash: "abc"}, true},
		{"区间为空", TrafficRef{TrafficID: "t1", SliceStart: 10, SliceEnd: 10, SliceHash: "abc"}, true},
		{"负起点", TrafficRef{TrafficID: "t1", SliceStart: -5, SliceEnd: 10, SliceHash: "abc"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateSliceRefs([]TrafficRef{c.ref})
			if c.wantErr && err == nil {
				t.Fatalf("应报错")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("不应报错: %v", err)
			}
		})
	}
}

// NormalizeTrafficRefs 是统一入口：切片形状错误必须在这里被拦下，
// 而不是等到落库或复核时才发现。
func TestNormalizeTrafficRefsRejectsBadSlice(t *testing.T) {
	_, err := NormalizeTrafficRefs([]TrafficRef{
		{TrafficID: "t1", Role: "proof", SliceStart: 10, SliceEnd: 20}, // 缺 hash
	})
	if err == nil {
		t.Fatal("统一入口应拦住缺 hash 的锚定")
	}
}

// 未锚定的历史引用必须继续能通过（向后兼容）。
func TestNormalizeTrafficRefsAcceptsLegacyUnanchored(t *testing.T) {
	out, err := NormalizeTrafficRefs([]TrafficRef{
		{TrafficID: "t1", Role: "proof", Note: "legacy"},
	})
	if err != nil {
		t.Fatalf("历史未锚定引用应通过: %v", err)
	}
	if len(out) != 1 || out[0].Anchored() {
		t.Fatalf("不应被标记为已锚定: %+v", out)
	}
}

// 摘要必须稳定且对内容敏感。
func TestSliceDigestStability(t *testing.T) {
	a := SliceDigest([]byte("hello"))
	b := SliceDigest([]byte("hello"))
	c := SliceDigest([]byte("hellp"))
	if a != b {
		t.Fatal("同内容应同摘要")
	}
	if a == c {
		t.Fatal("不同内容应不同摘要")
	}
	if len(a) != 16 {
		t.Fatalf("摘要应为 16 字符 hex，got %d: %q", len(a), a)
	}
}
