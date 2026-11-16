package webtest

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestReplayBaselineAndSinglePointContract(t *testing.T) {
	base := Request{Method: "POST", URL: "https://host.invalid/a?x=old#fragment", Headers: [][2]string{{"X-Token", "old"}, {"Cookie", "sid=old; theme=dark"}}, Body: []byte("username=old&name=old&tail=stay")}
	for _, m := range []Mutation{
		{Kind: "query", Name: "x", From: "wrong", To: "new"},
		{Kind: "header", Name: "X-Token", From: "wrong", To: "new"},
		{Kind: "cookie", Name: "sid", From: "wrong", To: "new"},
		{Kind: "body", Name: "missing", From: "old", To: "new"},
		{Kind: "body", Name: "name", From: "old", To: "old"},
		{Kind: "body", Name: "name", Replace: true, To: "new"},
		{Kind: "method", From: "GET", To: "PUT"},
		{Kind: "path", From: "host.invalid", To: "other.invalid"},
	} {
		if _, err := Replay(base, m); err == nil {
			t.Errorf("accepted invalid mutation: %+v", m)
		}
	}
	got, err := Replay(base, Mutation{Kind: "body", Name: "name", From: "old", To: "new"})
	if err != nil || string(got.Body) != "username=old&name=new&tail=stay" {
		t.Fatalf("body: %q, %v", got.Body, err)
	}
	got, err = Replay(base, Mutation{Kind: "query", Name: "x", From: "old", To: "new"})
	if err != nil || got.URL != "https://host.invalid/a?x=new#fragment" {
		t.Fatalf("query: %s, %v", got.URL, err)
	}
}

func TestReplayExplicitEmptyBaselineIsValidated(t *testing.T) {
	var m Mutation
	if err := json.Unmarshal([]byte(`{"kind":"query","name":"x","from":"","to":"new"}`), &m); err != nil {
		t.Fatal(err)
	}
	if _, err := Replay(Request{URL: "https://example.invalid/?x=old"}, m); err == nil {
		t.Fatal("explicit empty from ignored")
	}
	if _, err := Replay(Request{URL: "https://example.invalid/?x="}, m); err != nil {
		t.Fatal(err)
	}
}

func TestComparisonHeadersAndIncompleteEvidence(t *testing.T) {
	a := Response{Status: 302, Headers: http.Header{"Location": {"/home"}}}
	b := Response{Status: 302, Headers: http.Header{"location": {"/login"}}}
	if Compare(a, b).Verdict != VerdictDifferent || Digest(a) == Digest(b) {
		t.Fatal("redirect evidence lost")
	}
	if CompareWithNoise(a, b, b).Verdict != VerdictInconclusive || Stable(a, b) {
		t.Fatal("unstable headers accepted")
	}
	b.Headers = http.Header{"location": {"/home"}, "Date": {"later"}}
	if Compare(a, b).Verdict != VerdictSame || Digest(a) != Digest(b) {
		t.Fatal("header case/time noise")
	}
	for _, verdict := range []Verdict{VerdictDifferent, VerdictSame, VerdictInconclusive} {
		if (Comparison{Verdict: verdict}).FindingState() != "inconclusive" {
			t.Fatal("difference is not vulnerability proof")
		}
	}
	b.Truncated = true
	if Compare(a, b).Verdict != VerdictInconclusive || Stable(a, b) || Digest(a) == Digest(b) {
		t.Fatal("truncation was ignored")
	}
}
