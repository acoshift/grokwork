package filename

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"error.log":        "error.log",
		"../../etc/passwd": "passwd",
		"weird name!!.txt": "weird name__.txt",
		"":                 "file",
		".":                "file",
		"..":               "file",
		"a/b/c.png":        "c.png",
		"สวัสดี.txt":       "สวัสดี.txt",
		"กรุงเทพ.pdf":      "กรุงเทพ.pdf",
		"ข้อมูลลูกค้า.xlsx": "ข้อมูลลูกค้า.xlsx",
		"ที่อยู่จัดส่ง.csv": "ที่อยู่จัดส่ง.csv",
		"เบอร์โทรศัพท์.txt": "เบอร์โทรศัพท์.txt",
		"ภาษาไทย.txt":       "ภาษาไทย.txt",
	}
	for in, want := range cases {
		got := Sanitize(in)
		if got != want {
			t.Errorf("Sanitize(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSanitizeLongASCIIKeepsExtension(t *testing.T) {
	got := Sanitize(strings.Repeat("a", 200) + ".log")
	if len(got) > 120 {
		t.Fatalf("len=%d", len(got))
	}
	if want := strings.Repeat("a", 116) + ".log"; got != want {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizeLongThaiStaysValidUTF8(t *testing.T) {
	// ก้ is one cluster (base + tone mark, 6 bytes). Past the 120-byte cap
	// a byte slice lands inside a rune and drops the tone off the last base.
	in := strings.Repeat("ก้", 40) + ".pdf"
	got := Sanitize(in)
	if len(got) > 120 {
		t.Fatalf("len=%d", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("invalid utf-8: %q", got)
	}
	if !strings.HasSuffix(got, ".pdf") {
		t.Fatalf("extension dropped: %q", got)
	}
	body := strings.TrimSuffix(got, ".pdf")
	if body == "" || !strings.HasPrefix(strings.Repeat("ก้", 40), body) {
		t.Fatalf("body %q is not a cluster prefix", body)
	}
	if strings.HasSuffix(body, "ก") {
		t.Fatalf("tone mark split off the last base: %q", body)
	}
}
