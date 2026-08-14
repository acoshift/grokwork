package filestore

import "testing"

func TestValidateObjectPath(t *testing.T) {
	if err := ValidateObjectPath(""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateObjectPath("a/b"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateObjectPath("Report [final].pdf"); err != nil {
		t.Fatalf("bracket name: %v", err)
	}
	if err := ValidateObjectPath("x*"); err != nil {
		t.Fatalf("wildcard name is a Drive-legal leaf: %v", err)
	}
	if err := ValidateObjectPath("a%2Fb"); err != nil {
		t.Fatalf("encoded slash name: %v", err)
	}
	for _, bad := range []string{"/x", "a/../b", "a//b", "a/%2e%2e"} {
		if err := ValidateObjectPath(bad); err == nil {
			t.Fatalf("want error for %q", bad)
		}
	}
}

func TestJoinSplitPathRoundTrip(t *testing.T) {
	if got := JoinNames("Docs for Customer", "CR AMB"); got != "Docs for Customer/CR AMB" {
		t.Fatalf("nested = %q", got)
	}
	if got := JoinNames("Docs/Customer"); got != "Docs%2FCustomer" {
		t.Fatalf("slash name = %q", got)
	}
	if got := AppendName("", "a/b"); got != "a%2Fb" {
		t.Fatalf("append root = %q", got)
	}
	if got := AppendName("docs", "a/b"); got != "docs/a%2Fb" {
		t.Fatalf("append = %q", got)
	}
	segs, err := SplitPath("docs/a%2Fb")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 || segs[0] != "docs" || segs[1] != "a/b" {
		t.Fatalf("split = %#v", segs)
	}
	native, err := NativePath("docs/a%2Fb")
	if err != nil || native != "docs/a/b" {
		t.Fatalf("native = %q %v", native, err)
	}
}

func TestSanitizeFilename(t *testing.T) {
	if got := SanitizeFilename("../../etc/passwd"); got == "" || got == ".." {
		t.Fatalf("got %q", got)
	}
	if got := SanitizeFilename("ok-file.txt"); got != "ok-file.txt" {
		t.Fatalf("got %q", got)
	}
}
