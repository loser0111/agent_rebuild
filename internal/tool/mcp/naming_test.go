package mcp

import (
	"strings"
	"testing"
)

func TestFullNameRoundTrip(t *testing.T) {
	full := FullName("github", "search.issues/v2") // 原始名不清洗；即使含 "/" 也按第一个切
	if full != "github/search.issues/v2" {
		t.Fatalf("got %q", full)
	}
	s, tl, ok := SplitFullName(full)
	if !ok || s != "github" || tl != "search.issues/v2" {
		t.Fatalf("split = %q %q %v", s, tl, ok)
	}
	if _, _, ok := SplitFullName("no_slash"); ok {
		t.Fatal("没有 / 时应返回 ok=false")
	}
}

func TestValidateServerName(t *testing.T) {
	for _, ok := range []string{"github", "my-server", "a_b", "a__b", "A1-b_c"} {
		if err := ValidateServerName(ok); err != nil {
			t.Fatalf("%q 应合法：%v", ok, err)
		}
	}
	for _, bad := range []string{"", "a/b", "a.b", "中文", "a b", strings.Repeat("x", maxServerName+1)} {
		if err := ValidateServerName(bad); err == nil {
			t.Fatalf("%q 应不合法", bad)
		}
	}
}
