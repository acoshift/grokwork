package sessionstore

import "testing"

func TestCaseKeyPrefix(t *testing.T) {
	cases := map[string]string{
		"webapp":           "WEBAPP",
		"WebApp":           "WEBAPP",
		"my-app":           "MYAPP",
		"acme_web.service": "ACMEWEBSER", // capped at 10
		"api2":             "API2",
		"2fast":            "FAST", // leading digits cannot start a key
		"":                 "CASE",
		"123":              "CASE",
		"---":              "CASE",
		"ผู้ใช้":           "CASE", // no ASCII letters survive uppercasing meaningfully
	}
	for in, want := range cases {
		if got := CaseKeyPrefix(in); got != want {
			t.Errorf("CaseKeyPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCaseKeyPrefixIsIdempotent(t *testing.T) {
	// ParseCaseKey validates a prefix by re-deriving it, so derivation has to be
	// a fixed point or a legally-minted key would fail to parse.
	for _, in := range []string{"webapp", "my-app", "API2", "acme_web.service"} {
		once := CaseKeyPrefix(in)
		if twice := CaseKeyPrefix(once); twice != once {
			t.Errorf("CaseKeyPrefix not idempotent for %q: %q → %q", in, once, twice)
		}
	}
}

func TestParseAndNormalizeCaseKey(t *testing.T) {
	ok := map[string]string{
		"WEBAPP-14":  "WEBAPP-14",
		"webapp-14":  "WEBAPP-14",
		"  API2-1  ": "API2-1",
		"MY-APP-3":   "", // hyphen inside the prefix is not a legal prefix
		"WEBAPP-014": "", // no leading zeros: two spellings of one case
		"WEBAPP-0":   "",
		"WEBAPP--1":  "",
		"WEBAPP-":    "",
		"-14":        "",
		"WEBAPP":     "",
		"WEBAPP-1x":  "",
		"":           "",
		"WEBAPP-1-2": "",
	}
	for in, want := range ok {
		if got := NormalizeCaseKey(in); got != want {
			t.Errorf("NormalizeCaseKey(%q) = %q, want %q", in, got, want)
		}
	}
	if p, n, ok := ParseCaseKey("webapp-14"); !ok || p != "WEBAPP" || n != 14 {
		t.Fatalf("ParseCaseKey = %q,%d,%v", p, n, ok)
	}
}

func TestAllocateCaseKeyIsMonotonicAcrossDeletion(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.AllocateCaseKey("webapp")
	if err != nil {
		t.Fatal(err)
	}
	if first != "WEBAPP-1" {
		t.Fatalf("first key = %q, want WEBAPP-1", first)
	}
	if err := st.Set("t1", Entry{Mode: "case", Project: "webapp", CaseKey: first}); err != nil {
		t.Fatal(err)
	}
	second, _ := st.AllocateCaseKey("webapp")
	if second != "WEBAPP-2" {
		t.Fatalf("second key = %q, want WEBAPP-2", second)
	}
	// The whole point of a persisted mark: abandoning a case must not hand its
	// number to the next one, or every reference already written down re-aims.
	if err := st.Delete("t1"); err != nil {
		t.Fatal(err)
	}
	third, _ := st.AllocateCaseKey("webapp")
	if third != "WEBAPP-3" {
		t.Fatalf("after delete, key = %q, want WEBAPP-3", third)
	}
	// A different prefix has its own sequence.
	if k, _ := st.AllocateCaseKey("api"); k != "API-1" {
		t.Fatalf("other prefix = %q, want API-1", k)
	}
}

func TestAllocateCaseKeySurvivesLostCounter(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("t9", Entry{Mode: "case", Project: "webapp", CaseKey: "WEBAPP-9"}); err != nil {
		t.Fatal(err)
	}
	// Fresh store, no case-seq.json written yet: the mark has to be raised past
	// keys already in the store or it would re-issue WEBAPP-1.
	if k, _ := st.AllocateCaseKey("webapp"); k != "WEBAPP-10" {
		t.Fatalf("key = %q, want WEBAPP-10", k)
	}
}

func TestFindByCaseKey(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("thread-a", Entry{Mode: "case", Project: "webapp", CaseKey: "WEBAPP-4"}); err != nil {
		t.Fatal(err)
	}
	id, e, ok := st.FindByCaseKey("webapp-4")
	if !ok || id != "thread-a" || e.Project != "webapp" {
		t.Fatalf("FindByCaseKey = %q,%v,%v", id, e.Project, ok)
	}
	if _, _, ok := st.FindByCaseKey("WEBAPP-5"); ok {
		t.Fatal("unknown key must not resolve")
	}
	if _, _, ok := st.FindByCaseKey("nonsense"); ok {
		t.Fatal("malformed key must not resolve")
	}
}

func TestRelatedCaseKeysCanonicalises(t *testing.T) {
	e := Entry{
		CaseKey:      "WEBAPP-4",
		RelatedCases: []string{"webapp-9", "WEBAPP-9", " API-1 ", "WEBAPP-4", "garbage", ""},
	}
	got := e.RelatedCaseKeys()
	want := []string{"WEBAPP-9", "API-1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
