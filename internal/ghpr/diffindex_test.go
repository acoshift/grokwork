package ghpr

import (
	"context"
	"strings"
	"testing"
)

func TestBuildIndexNumstatNameStatus(t *testing.T) {
	numstat := []byte("0\t0\t\x00a.txt\x00b.txt\x00-\t-\timg.bin\x002\t1\tkeep.txt\x005\t0\tnew.go\x000\t3\tgone.go\x00")
	nameStatus := []byte("R100\x00a.txt\x00b.txt\x00M\x00img.bin\x00M\x00keep.txt\x00A\x00new.go\x00D\x00gone.go\x00")
	d := buildIndex(numstat, nameStatus)
	if len(d.Files) != 5 {
		t.Fatalf("files = %d, want 5", len(d.Files))
	}
	byPath := map[string]FileStat{}
	for _, f := range d.Files {
		byPath[f.Path] = f
	}
	if f := byPath["b.txt"]; f.Status != "R" || f.OldPath != "a.txt" {
		t.Fatalf("rename = %+v", f)
	}
	if f := byPath["img.bin"]; !f.Binary {
		t.Fatalf("binary = %+v", f)
	}
	if f := byPath["keep.txt"]; f.Status != "M" || f.Adds != 2 || f.Dels != 1 {
		t.Fatalf("modified = %+v", f)
	}
	if f := byPath["new.go"]; f.Status != "A" || f.Adds != 5 {
		t.Fatalf("added = %+v", f)
	}
	if f := byPath["gone.go"]; f.Status != "D" || f.Dels != 3 {
		t.Fatalf("deleted = %+v", f)
	}
	if d.TotalAdds != 7 || d.TotalDels != 4 {
		t.Fatalf("totals = +%d -%d, want +7 -4", d.TotalAdds, d.TotalDels)
	}
}

const statPatchFixture = `diff --git a/old.go b/renamed.go
similarity index 90%
rename from old.go
rename to renamed.go
--- a/old.go
+++ b/renamed.go
@@ -1,3 +1,3 @@
 ctx
-removed
+added
diff --git a/fresh.go b/fresh.go
new file mode 100644
--- /dev/null
+++ b/fresh.go
@@ -0,0 +1,2 @@
+one
+two
diff --git a/dead.go b/dead.go
deleted file mode 100644
--- a/dead.go
+++ /dev/null
@@ -1 +0,0 @@
-bye
diff --git a/pic.png b/pic.png
Binary files a/pic.png and b/pic.png differ
`

func TestStatPatch(t *testing.T) {
	d := StatPatch([]byte(statPatchFixture), 0)
	if len(d.Files) != 4 {
		t.Fatalf("files = %d, want 4", len(d.Files))
	}
	byPath := map[string]FileStat{}
	for _, f := range d.Files {
		byPath[f.Path] = f
	}
	if f := byPath["renamed.go"]; f.Status != "R" || f.OldPath != "old.go" || f.Adds != 1 || f.Dels != 1 {
		t.Fatalf("rename = %+v", f)
	}
	if f := byPath["fresh.go"]; f.Status != "A" || f.Adds != 2 || f.OldPath != "" {
		t.Fatalf("added = %+v", f)
	}
	if f := byPath["dead.go"]; f.Status != "D" || f.Dels != 1 {
		t.Fatalf("deleted = %+v", f)
	}
	if f := byPath["pic.png"]; !f.Binary {
		t.Fatalf("binary = %+v", f)
	}
	if d.TotalAdds != 3 || d.TotalDels != 2 {
		t.Fatalf("totals = +%d -%d, want +3 -2", d.TotalAdds, d.TotalDels)
	}
}

func TestStatPatchMaxFiles(t *testing.T) {
	var b strings.Builder
	for range 4 {
		b.WriteString(statPatchFixture)
	}
	d := StatPatch([]byte(b.String()), 10)
	if !d.Truncated {
		t.Fatal("want Truncated")
	}
	if len(d.Files) != 10 {
		t.Fatalf("files = %d, want 10", len(d.Files))
	}
}

func TestExtractFilePatch(t *testing.T) {
	sec := ExtractFilePatch([]byte(statPatchFixture), "fresh.go")
	if sec == nil {
		t.Fatal("fresh.go not found")
	}
	s := string(sec)
	if !strings.HasPrefix(s, "diff --git a/fresh.go b/fresh.go\n") {
		t.Fatalf("section start = %q", s[:40])
	}
	if strings.Contains(s, "dead.go") {
		t.Fatal("section leaked into next file")
	}
	// Rename matches by old path too.
	if ExtractFilePatch([]byte(statPatchFixture), "old.go") == nil {
		t.Fatal("old.go (rename old side) not found")
	}
	// Last section extends to EOF.
	if sec := ExtractFilePatch([]byte(statPatchFixture), "pic.png"); sec == nil || !strings.Contains(string(sec), "Binary files") {
		t.Fatalf("pic.png section = %q", sec)
	}
	if ExtractFilePatch([]byte(statPatchFixture), "missing.go") != nil {
		t.Fatal("missing.go should be nil")
	}
}

func TestFileDiffRunnersPassPathspec(t *testing.T) {
	var gotArgs []string
	run := func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		gotArgs = append(gotArgs, name+" "+strings.Join(args, " "))
		if name == "git" && len(args) > 0 && args[0] == "merge-base" {
			return []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"), nil
		}
		return []byte("diff --git a/old.go b/renamed.go\n--- a/old.go\n+++ b/renamed.go\n@@ -1 +1 @@\n-x\n+y\n"), nil
	}
	d, err := ShowCommitFileWith(context.Background(), run, "/repo", "abc", "renamed.go", "old.go", FileCaps())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(gotArgs, " | ")
	if !strings.Contains(joined, "git show --format= -p --no-ext-diff abc -- renamed.go old.go") {
		t.Fatalf("pathspec args = %q", joined)
	}
	if len(d.Files) != 1 || d.Files[0].PathNew != "renamed.go" {
		t.Fatalf("diff = %+v", d)
	}

	gotArgs = nil
	if _, err := WorktreeDiffFileWith(context.Background(), run, "/wt", "origin/main", "renamed.go", "", FileCaps()); err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(gotArgs, " | ")
	if !strings.Contains(joined, "git merge-base origin/main HEAD") {
		t.Fatalf("want merge-base call in %q", joined)
	}
	if !strings.Contains(joined, "git diff bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb -- renamed.go") {
		t.Fatalf("worktree args = %q", joined)
	}

	if _, err := WorktreeDiffFileWith(context.Background(), run, "", "x", "p", "", FileCaps()); err == nil {
		t.Fatal("empty cwd must error")
	}
}

func TestPRPatchWith(t *testing.T) {
	run := func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		if name != "gh" || args[0] != "pr" || args[1] != "diff" {
			t.Fatalf("unexpected cmd %s %v", name, args)
		}
		return []byte(statPatchFixture), nil
	}
	raw, err := PRPatchWith(context.Background(), run, "/repo", "https://github.com/a/b/pull/1")
	if err != nil || len(raw) == 0 {
		t.Fatalf("raw=%d err=%v", len(raw), err)
	}
	// The raw patch composes with ExtractFilePatch + ParseUnifiedDiff.
	d := ParseUnifiedDiff(ExtractFilePatch(raw, "fresh.go"), FileCaps())
	if len(d.Files) != 1 || d.Files[0].PathNew != "fresh.go" || len(d.Files[0].Hunks) != 1 {
		t.Fatalf("diff = %+v", d)
	}
	// Missing file → nil section → empty diff.
	if d := ParseUnifiedDiff(ExtractFilePatch(raw, "missing.go"), FileCaps()); len(d.Files) != 0 {
		t.Fatalf("missing = %+v", d)
	}
	if _, err := PRPatchWith(context.Background(), run, "/repo", " "); err == nil {
		t.Fatal("empty selector must error")
	}
}

func TestRenderHunks(t *testing.T) {
	f := DiffFile{
		PathNew: "x.go",
		Hunks: []Hunk{
			{Header: "@@ -10,3 +20,4 @@ func X", Lines: []string{" ctx1", "-del1", "+add1", "+add2", " ctx2", "\\ No newline at end of file"}},
			{Header: "@@ -30 +41 @@", Lines: []string{"-only", "+only"}},
		},
	}
	hs := RenderHunks(f)
	if len(hs) != 2 {
		t.Fatalf("hunks = %d", len(hs))
	}
	l := hs[0].Lines
	if l[0].Old != 10 || l[0].New != 20 || l[0].Kind != "ctx" {
		t.Fatalf("ctx1 = %+v", l[0])
	}
	if l[1].Old != 11 || l[1].New != 0 || l[1].Kind != "del" {
		t.Fatalf("del1 = %+v", l[1])
	}
	if l[2].Old != 0 || l[2].New != 21 || l[2].Kind != "add" {
		t.Fatalf("add1 = %+v", l[2])
	}
	if l[3].New != 22 || l[4].Old != 12 || l[4].New != 23 {
		t.Fatalf("add2/ctx2 = %+v %+v", l[3], l[4])
	}
	if l[5].Kind != "meta" || l[5].Old != 0 || l[5].New != 0 {
		t.Fatalf("meta = %+v", l[5])
	}
	// Count-omitted header form.
	l = hs[1].Lines
	if l[0].Old != 30 || l[1].New != 41 {
		t.Fatalf("hunk2 = %+v %+v", l[0], l[1])
	}
	// Unparsable header → numbers stay 0.
	bad := RenderHunks(DiffFile{Hunks: []Hunk{{Header: "@@@ weird", Lines: []string{"+x"}}}})
	if bl := bad[0].Lines[0]; bl.New != 0 || bl.Kind != "add" {
		t.Fatalf("bad header line = %+v", bl)
	}
}
