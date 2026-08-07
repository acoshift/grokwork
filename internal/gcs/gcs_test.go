package gcs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestValidateObjectPath(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"", true},
		{"file.txt", true},
		{"a/b/c", true},
		{"a-b_c.d", true},
		{"/leading", false},
		{"a/../b", false},
		{"a/./b", false},
		{"a//b", false},
		{"foo*", false},
		{"foo?", false},
		{"foo[0]", false},
		{"a\nb", false},
		{"a\x00b", false},
		{strings.Repeat("x", maxObjectPathBytes), true},
		{strings.Repeat("x", maxObjectPathBytes+1), false},
	}
	for _, tc := range cases {
		err := ValidateObjectPath(tc.in)
		if tc.ok && err != nil {
			t.Errorf("ValidateObjectPath(%q)=%v want nil", tc.in, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("ValidateObjectPath(%q)=nil want error", tc.in)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"report.pdf":                      "report.pdf",
		"../../etc/passwd":                "passwd",
		"weird name!!!.txt":               "weird name___.txt",
		"":                                "file",
		".":                               "file",
		"..":                              "file",
		strings.Repeat("a", 200) + ".log": strings.Repeat("a", 116) + ".log",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q)=%q want %q", in, got, want)
		}
	}
}

func TestListArgvAndParse(t *testing.T) {
	var gotArgs []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name != Binary {
			t.Fatalf("binary=%q", name)
		}
		gotArgs = append([]string{}, args...)
		return []byte(`[
			{"url":"gs://b/pfx/a.txt","type":"cloud_object","metadata":{"name":"pfx/a.txt","size":"42","updated":"2024-01-02T03:04:05Z","contentType":"text/plain"}},
			{"url":"gs://b/pfx/folder/","type":"prefix"},
			{"url":"gs://b/pfx/num.bin","metadata":{"size":99}},
			{"url":"gs://b/pfx/bare"}
		]`), nil
	}
	entries, err := ListWith(t.Context(), run, "b", "pfx", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(gotArgs, " ") != "storage ls --json gs://b/pfx/" {
		t.Fatalf("args=%v", gotArgs)
	}
	if len(entries) != 4 {
		t.Fatalf("len=%d %+v", len(entries), entries)
	}
	if entries[0].Name != "a.txt" || entries[0].IsDir || entries[0].Size != 42 || entries[0].ContentType != "text/plain" {
		t.Fatalf("file0=%+v", entries[0])
	}
	if !entries[0].Updated.Equal(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("updated=%v", entries[0].Updated)
	}
	if entries[1].Name != "folder" || !entries[1].IsDir {
		t.Fatalf("folder=%+v", entries[1])
	}
	if entries[2].Name != "num.bin" || entries[2].Size != 99 {
		t.Fatalf("num=%+v", entries[2])
	}
	if entries[3].Name != "bare" || entries[3].Size != 0 {
		t.Fatalf("bare=%+v", entries[3])
	}
}

func TestListSubPath(t *testing.T) {
	var gotArgs []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`[]`), nil
	}
	if _, err := ListWith(t.Context(), run, "b", "pfx", "sub"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(gotArgs, " ") != "storage ls --json gs://b/pfx/sub/" {
		t.Fatalf("args=%v", gotArgs)
	}
}

func TestDescribeArgvAndNotFound(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		want := "storage objects describe gs://b/pfx/a.txt --format=json"
		if strings.Join(args, " ") != want {
			t.Fatalf("args=%v want %s", args, want)
		}
		return []byte(`{"name":"pfx/a.txt","size":"10","contentType":"text/plain"}`), nil
	}
	e, ok, err := DescribeWith(t.Context(), run, "b", "pfx", "a.txt")
	if err != nil || !ok {
		t.Fatalf("err=%v ok=%v", err, ok)
	}
	if e.Name != "a.txt" || e.Size != 10 || e.ContentType != "text/plain" {
		t.Fatalf("entry=%+v", e)
	}

	missing := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("gcloud %s: ERROR: 404 Not Found: No such object", strings.Join(args, " "))
	}
	_, ok, err = DescribeWith(t.Context(), missing, "b", "pfx", "gone.txt")
	if err != nil || ok {
		t.Fatalf("missing: err=%v ok=%v", err, ok)
	}
}

func TestUploadDownloadDeleteArgv(t *testing.T) {
	var last []string
	capture := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		last = append([]string{}, args...)
		return nil, nil
	}
	if err := UploadWith(t.Context(), capture, "/tmp/f", "b", "pfx", "a.txt"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(last, " ") != "storage cp /tmp/f gs://b/pfx/a.txt" {
		t.Fatalf("upload args=%v", last)
	}
	if err := DownloadWith(t.Context(), capture, "b", "pfx", "a.txt", "/tmp/out"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(last, " ") != "storage cp gs://b/pfx/a.txt /tmp/out" {
		t.Fatalf("download args=%v", last)
	}
	if err := DeleteWith(t.Context(), capture, "b", "pfx", "a.txt"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(last, " ") != "storage rm gs://b/pfx/a.txt" {
		t.Fatalf("delete args=%v", last)
	}
}

func TestDeleteRefusals(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatal("runner must not be called")
		return nil, nil
	}
	// Wildcard in object name is caught by ValidateObjectPath first.
	if err := DeleteWith(t.Context(), run, "b", "", "a*"); err == nil {
		t.Fatal("want wildcard refusal")
	}
	// Empty object.
	if err := DeleteWith(t.Context(), run, "b", "pfx", ""); err == nil {
		t.Fatal("want empty object refusal")
	}
	// objectURL never emits a trailing-slash object URL (folders are list-only).
	if got := objectURL("b", "folder/"); strings.HasSuffix(got, "/") && got != "gs://b/" {
		t.Fatalf("objectURL must not leave a trailing slash on an object: %q", got)
	}
}

func TestObjectURLJoin(t *testing.T) {
	if got := objectURL("b", joinObject("pfx", "a/b.txt")); got != "gs://b/pfx/a/b.txt" {
		t.Fatalf("got %q", got)
	}
	if got := listURL("b", ""); got != "gs://b/" {
		t.Fatalf("root list %q", got)
	}
	if got := listURL("b", "pfx"); got != "gs://b/pfx/" {
		t.Fatalf("pfx list %q", got)
	}
}
