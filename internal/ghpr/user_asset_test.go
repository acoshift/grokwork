package ghpr

import (
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestUserAssetURLAllowed(t *testing.T) {
	ok := []string{
		"https://github.com/user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"https://github.com/user-attachments/assets/abcd/",
		"https://www.github.com/user-attachments/assets/abcd",
		"https://github.com/user-attachments/files/12345/shot.png",
		"https://user-images.githubusercontent.com/1/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.png",
		"https://private-user-images.githubusercontent.com/1/x.png?jwt=abc",
		"https://objects.githubusercontent.com/github-production-user-asset-6210df/1/2",
		"https://github-production-user-asset-6210df.s3.amazonaws.com/110723939/636911705-c44c50cd.png",
		"https://github-production-user-asset-6210df.s3.us-east-1.amazonaws.com/1/x.png?X-Amz-Algorithm=AWS4-HMAC-SHA256",
	}
	for _, u := range ok {
		if !UserAssetURLAllowed(u) {
			t.Fatalf("allowed: %s", u)
		}
	}
	deny := []string{
		"",
		"http://github.com/user-attachments/assets/abcd",
		"https://github.com/acme/app/raw/main/secret.env",
		"https://github.com/acme/app/archive/refs/heads/main.zip",
		"https://github.com/user-attachments/assets/../secrets",
		"https://github.com/user-attachments/assets/abcd/extra",
		"https://github.com/user-attachments/files/12345/notes.txt",
		"https://github.com:8443/user-attachments/assets/abcd",
		"https://evil.com/user-attachments/assets/abcd",
		"https://camo.githubusercontent.com/abc",
		"https://raw.githubusercontent.com/acme/app/main/a.png",
		"https://169.254.169.254/latest/meta-data",
		"https://user:pass@github.com/user-attachments/assets/abcd",
		"https://evil-bucket.s3.amazonaws.com/secret.png",
		"https://github-production-release-asset-6210df.s3.amazonaws.com/1/x.png",
		"https://github-production-user-asset-6210df.s3.amazonaws.com.evil.com/x.png",
		"https://github-production-user-asset-NOTHEX.s3.amazonaws.com/x.png",
	}
	for _, u := range deny {
		if UserAssetURLAllowed(u) {
			t.Fatalf("denied: %s", u)
		}
	}
}

func TestExtractUserAssetURLs(t *testing.T) {
	src := `see

<img width="800" alt="repro" src="https://github.com/user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" />

and again ![x](https://github.com/user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee)

plus https://example.com/a.png and https://github.com/acme/app/raw/main/x.png
`
	got := ExtractUserAssetURLs(src)
	if len(got) != 1 || !strings.Contains(got[0], "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee") {
		t.Fatalf("%v", got)
	}
}

func TestExtractUserAssetURLsCap(t *testing.T) {
	var b strings.Builder
	for i := range MaxUserAssetsPerBody + 3 {
		b.WriteString("https://github.com/user-attachments/assets/")
		b.WriteByte('a' + byte(i))
		b.WriteString("\n")
	}
	got := ExtractUserAssetURLs(b.String())
	if len(got) != MaxUserAssetsPerBody {
		t.Fatalf("cap: got %d", len(got))
	}
}

func TestNewUserAssetRequestHeaders(t *testing.T) {
	req, err := newUserAssetRequest(t.Context(), "tok", "https://github.com/user-attachments/assets/abcd")
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("User-Agent"); got != userAssetUserAgent {
		t.Fatalf("ua=%q", got)
	}
	if got := req.Header.Get("Accept"); got != userAssetAccept {
		t.Fatalf("accept=%q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("auth=%q", got)
	}
}

func TestUserAssetRetryable(t *testing.T) {
	if !userAssetRetryable(fmt.Errorf("github image: HTTP 503")) {
		t.Fatal("503 should retry")
	}
	if userAssetRetryable(fmt.Errorf("github image: HTTP 404")) {
		t.Fatal("404 should not retry")
	}
}

func TestPublicIP(t *testing.T) {
	if publicIP(net.ParseIP("10.0.0.1")) || publicIP(net.ParseIP("127.0.0.1")) ||
		publicIP(net.ParseIP("169.254.169.254")) || publicIP(net.ParseIP("::1")) {
		t.Fatal("private/loopback/link-local must fail")
	}
	if !publicIP(net.ParseIP("1.1.1.1")) {
		t.Fatal("public v4 must pass")
	}
}
