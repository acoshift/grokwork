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
	for _, bad := range []string{"/x", "a/../b", "a//b"} {
		if err := ValidateObjectPath(bad); err == nil {
			t.Fatalf("want error for %q", bad)
		}
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
