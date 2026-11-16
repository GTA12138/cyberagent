package authstate

import (
	"strings"
	"testing"
)

func TestPutAssignsVersionAndBumpsOnReplace(t *testing.T) {
	s := NewStore()
	first, err := s.Put(Identity{Principal: "admin@corp", Cookies: map[string]string{"sid": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 {
		t.Fatalf("首次写入应为 v1，got %d", first.Version)
	}
	second, err := s.Put(Identity{Principal: "admin@corp", Cookies: map[string]string{"sid": "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != 2 {
		t.Fatalf("替换必须递增版本，got %d（否则基于旧会话的结论看起来仍然成立）", second.Version)
	}
	if second.Ref() != "admin@corp@2" {
		t.Fatalf("Ref 格式错: %s", second.Ref())
	}
}

func TestPutRejectsEmptyPrincipal(t *testing.T) {
	if _, err := NewStore().Put(Identity{Principal: "  "}); err == nil {
		t.Fatal("空 principal 必须被拒：身份没有名字就无法溯源")
	}
}

func TestBumpInvalidatesEarlierConclusions(t *testing.T) {
	s := NewStore()
	_, _ = s.Put(Identity{Principal: "low"})
	stale, why := s.Stale("low@1")
	if stale {
		t.Fatalf("刚写入的引用不该过期: %s", why)
	}
	s.Bump("low")
	stale, why = s.Stale("low@1")
	if !stale {
		t.Fatal("凭据变更后旧引用必须过期")
	}
	if !strings.Contains(why, "v1") || !strings.Contains(why, "v2") {
		t.Fatalf("原因应指出新旧版本，got %q", why)
	}
}

func TestStaleForUnknownOrEmptyRef(t *testing.T) {
	s := NewStore()
	if stale, why := s.Stale(""); !stale || why == "" {
		t.Fatal("空引用必须判过期并给原因")
	}
	if stale, _ := s.Stale("ghost@1"); !stale {
		t.Fatal("不存在的身份必须判过期")
	}
	if stale, _ := s.Stale("x@notanumber"); !stale {
		t.Fatal("版本号无法解析必须判过期")
	}
}

func TestHeaderPairsMergesCookiesDeterministically(t *testing.T) {
	id := Identity{
		Headers: map[string]string{"X-API-Key": "k", "Accept": "json"},
		Cookies: map[string]string{"b": "2", "a": "1"},
	}
	pairs := id.HeaderPairs()
	if len(pairs) != 3 {
		t.Fatalf("两个头 + 一个合并的 Cookie 头，got %d: %v", len(pairs), pairs)
	}
	last := pairs[len(pairs)-1]
	if last[0] != "Cookie" {
		t.Fatalf("Cookie 应最后合并，got %v", last)
	}
	if last[1] != "a=1; b=2" {
		t.Fatalf("Cookie 顺序必须固定（顺序变了会让服务端指纹/缓存变化），got %q", last[1])
	}
}

func TestHasRoleIsCaseInsensitive(t *testing.T) {
	id := Identity{Roles: []string{"Admin", "auditor"}}
	if !id.HasRole("admin") || !id.HasRole("AUDITOR") {
		t.Fatal("角色判断应大小写不敏感")
	}
	if id.HasRole("root") {
		t.Fatal("不存在的角色不能误判")
	}
}

func TestCloneIsDeep(t *testing.T) {
	s := NewStore()
	orig, _ := s.Put(Identity{Principal: "p", Cookies: map[string]string{"sid": "a"}, Roles: []string{"user"}})
	orig.Cookies["sid"] = "mutated"
	orig.Roles[0] = "admin"
	got, _ := s.Get("p")
	if got.Cookies["sid"] != "a" || got.Roles[0] != "user" {
		t.Fatal("Store 必须返回深拷贝：外部改一个 map 不该改掉账本里的身份")
	}
}

func TestListIsSorted(t *testing.T) {
	s := NewStore()
	_, _ = s.Put(Identity{Principal: "z"})
	_, _ = s.Put(Identity{Principal: "a"})
	list := s.List()
	if len(list) != 2 || list[0].Principal != "a" {
		t.Fatalf("List 必须按 principal 升序，got %+v", list)
	}
}
