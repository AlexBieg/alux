package scrub

import (
	"strings"
	"testing"
)

func TestPlutilDumpHasItsSecretValuesRedacted(t *testing.T) {
	dump := `{
  "VendorToken" => "cgr8Hq2LmZ0pX7vT4kW9"
  "ServiceXPassword" => "Px9-qWl3mZ7rT2vKa"
  "ServerURL" => "https://example.internal"
  "Enabled" => 1
}`
	got := Text(dump)
	for _, secret := range []string{"cgr8Hq2LmZ0pX7vT4kW9", "Px9-qWl3mZ7rT2vKa"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q survived:\n%s", secret, got)
		}
	}
	for _, kept := range []string{`"VendorToken" => "[redacted secret]"`, `"ServiceXPassword" => "[redacted secret]"`, "https://example.internal", `"Enabled" => 1`} {
		if !strings.Contains(got, kept) {
			t.Errorf("want %q in:\n%s", kept, got)
		}
	}
}

func TestKeyValueFormsRedactOnlyTheValue(t *testing.T) {
	cases := map[string]string{
		"GITHUB_TOKEN=Zq81mPx0Lw2nVb7c":                                     "GITHUB_TOKEN=[redacted secret]",
		"export API_KEY=Zq81mPx0Lw2nVb7c":                                   "export API_KEY=[redacted secret]",
		`{"client_secret": "Zq81mPx0Lw2nVb7c", "name": "x"}`:                `{"client_secret": "[redacted secret]", "name": "x"}`,
		"password: Zq81mPx0Lw2nVb7c":                                        "password: [redacted secret]",
		"<key>AuthToken</key>\n\t<string>Zq81mPx0Lw2nVb7c</string>":         "<key>AuthToken</key>\n\t<string>[redacted secret]</string>",
		"curl 'https://api.x.com/v1?user=me&access_token=Zq81mPx0Lw2nVb7c'": "curl 'https://api.x.com/v1?user=me&access_token=[redacted secret]'",
	}
	for in, want := range cases {
		if got := Text(in); got != want {
			t.Errorf("Text(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestKnownCredentialShapesAreRedacted(t *testing.T) {
	cases := map[string]string{
		"jwt":          "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"github token": "ghp_" + strings.Repeat("aB3", 12),
		"aws key":      "AKIAIOSFODNN7EXAMPLE",
		"anthropic":    "sk-ant-api03-" + strings.Repeat("Xy9", 10),
		"slack":        "xoxb-1234567890-abcdefghij",
		"gitlab":       "glpat-" + strings.Repeat("k2", 10),
		"bearer":       "Bearer abcDEF1234567890xyz",
	}
	for name, secret := range cases {
		got := Text("value: " + secret + " end")
		if strings.Contains(got, secret) || !strings.Contains(got, "[redacted ") {
			t.Errorf("%s not redacted: %q", name, got)
		}
	}
}

func TestPrivateKeyBlockIsRedacted(t *testing.T) {
	in := "before\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmU=\nAAAAC3NzaC1lZDI1NTE5\n-----END OPENSSH PRIVATE KEY-----\nafter"
	want := "before\n-----BEGIN OPENSSH PRIVATE KEY-----[redacted private key]-----END OPENSSH PRIVATE KEY-----\nafter"
	if got := Text(in); got != want {
		t.Errorf("got %q", got)
	}
}

func TestURLPasswordIsRedactedAndUserKept(t *testing.T) {
	in := "remote: https://alex:s3cretPass@github.com/org/repo.git"
	want := "remote: https://alex:[redacted password]@github.com/org/repo.git"
	if got := Text(in); got != want {
		t.Errorf("got %q", got)
	}
}

func TestOwnEnvironmentValuesAreRedactedAnywhere(t *testing.T) {
	t.Setenv("ALUX_TEST_SERVICE_TOKEN", "q7Rz2kLm")
	got := Text("the header was X-Thing q7Rz2kLm; again:q7Rz2kLm")
	if strings.Contains(got, "q7Rz2kLm") {
		t.Errorf("env value survived: %q", got)
	}
	if !strings.Contains(got, "[redacted ALUX_TEST_SERVICE_TOKEN]") {
		t.Errorf("got %q", got)
	}
}

func TestOrdinaryTextIsLeftAlone(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "Alex Smith 2026")
	t.Setenv("SSH_AUTH_SOCK", "/private/tmp/com.apple.launchd.k3j/Listeners")
	keep := []string{
		"commit 9fceb02d0ae598e95dc970b74767f19372d61af8",
		"Author: Alex Smith <alex@example.com>",
		"GIT_AUTHOR_NAME=Alex Smith 2026",
		"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"image digest: sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"@  kxqpmzwvnlrs alex 2026-10-25 1a2b3c4d",
		"Working copy  (@) : zzmqtnvsywlpkrxo 7f3e9a1c (no description set)",
		"golang.org/x/text v0.14.0 h1:ScX5w1eTa3QqT8oi6+ziP7dTV1S2+ALU0bI+0zXKWiQ=",
		"id 123e4567-e89b-12d3-a456-426614174000",
		"the token count is 5",
		"apiKey: process.env.API_KEY2",
		"accessToken = resp.AccessToken2",
		"input_tokens: 123456789012",
		"Password: cfg.Password,",
		"Token: tokenFromEnv(ctx),",
		"password=${DB_PASSWORD_VALUE}",
		"api_key: $OPENAI_API_KEY",
		`"token": "<redacted>"`,
		"secret: ************",
		"PASSWORD=changeme-12345",
		"auth_file=/Users/someone/.config/gh/hosts.yml",
		"/Users/someone/.local/share/alux/layers/alux-task-1792957222-1/internal",
		"cd risk-assessment-2026-plan-review-board",
		"SSH_AUTH_SOCK=/private/tmp/com.apple.launchd.k3j/Listeners",
	}
	for _, s := range keep {
		if got := Text(s); got != s {
			t.Errorf("changed:\n  in  %q\n  out %q", s, got)
		}
	}
}
