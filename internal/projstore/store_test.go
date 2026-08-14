package projstore

import (
	"bytes"
	"strings"
	"testing"
)

func TestValidateKey(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "../x", "/abs", "a//b", "a/./b", "a/../b", "has space", "ü"} {
		if err := ValidateKey(bad); err == nil {
			t.Fatalf("expected reject %q", bad)
		}
	}
	if err := ValidateKey("reports/case-1/out.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestPutGetListQuotaAndEscape(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.maxObjectBytes = 100
	s.maxProjectBytes = 150

	meta, err := s.Put("app", "shared/a.txt", []byte("hello"), "text/plain", "t1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Size != 5 || meta.Key != "shared/a.txt" {
		t.Fatalf("%+v", meta)
	}
	data, m2, err := s.Get("app", "shared/a.txt")
	if err != nil || !bytes.Equal(data, []byte("hello")) || m2.Key != meta.Key {
		t.Fatalf("get: %v %q %+v", err, data, m2)
	}
	// Other project cannot see it via different project root.
	if _, _, err := s.Get("other", "shared/a.txt"); err == nil {
		t.Fatal("expected missing in other project")
	}
	list, err := s.List("app", "shared/", 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %+v", err, list)
	}
	// Over object size.
	if _, err := s.Put("app", "big", bytes.Repeat([]byte("x"), 101), "", "", ""); err == nil {
		t.Fatal("expected object size reject")
	}
	// Quota: 5 used, max 150 — fill to exceed.
	if _, err := s.Put("app", "b", bytes.Repeat([]byte("y"), 100), "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put("app", "c", bytes.Repeat([]byte("z"), 50), "", "", ""); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("expected quota reject, got %v", err)
	}
	// Path-escape key
	if _, err := s.Put("app", "../etc/passwd", []byte("x"), "", "", ""); err == nil {
		t.Fatal("expected key reject")
	}
	if err := s.Delete("app", "shared/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get("app", "shared/a.txt"); err == nil {
		t.Fatal("expected gone")
	}
}
