// Package scrub redacts credentials from text that comes into a conversation from outside it.
package scrub

import (
	"os"
	"regexp"
	"sort"
	"strings"
)

var (
	keyName = regexp.MustCompile(`(?i)token|secret|password|passwd|api[_-]?key|private[_-]?key|access[_-]?key|credential|auth`)

	pem = regexp.MustCompile(`(-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----)(?s:.*?)(-----END [A-Z0-9 ]*PRIVATE KEY-----)`)

	urlCredential = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^\s:/@]+:)([^\s@/]+)@`)

	bearer = regexp.MustCompile(`(?i)\b(bearer\s+)([A-Za-z0-9._~+/=-]{16,})`)

	keyValue = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])(\\?["'])?([A-Za-z_][A-Za-z0-9_.-]*)(\\?["'])?(\s*(?:=>|:=|=|:)[ \t]*)(\\?["'])?([^\s"'\\<>,;&` + "`" + `=][^\s"'\\<>,;&` + "`" + `]*)(\\?["'])?`)

	// selector is code reading a field, such as process.env.API_KEY2, not a value.
	selector = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+$`)

	plistKey = regexp.MustCompile(`(<key>\s*)([^<]*?)(\s*</key>\s*<string>)([^<]*)(</string>)`)
)

type prefix struct {
	kind  string
	re    *regexp.Regexp
	digit bool
}

var prefixes = []prefix{
	{kind: "github token", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])(gh[pousr]_[A-Za-z0-9]{36,})`)},
	{kind: "github token", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])(github_pat_[A-Za-z0-9_]{22,})`)},
	{kind: "anthropic key", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])(sk-ant-[A-Za-z0-9_-]{20,})`)},
	{kind: "openai key", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])(sk-(?:proj-)?[A-Za-z0-9_-]{20,})`), digit: true},
	{kind: "slack token", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])(xox[abpr]-[A-Za-z0-9-]{10,})`), digit: true},
	{kind: "aws key", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])((?:AKIA|ASIA)[A-Z0-9]{16})`)},
	{kind: "google key", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])(AIza[A-Za-z0-9_-]{35})`)},
	{kind: "gitlab token", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])(glpat-[A-Za-z0-9_-]{20,})`)},
	{kind: "npm token", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])(npm_[A-Za-z0-9]{36,})`)},
	{kind: "linear key", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])(lin_api_[A-Za-z0-9]{32,})`)},
	{kind: "jwt", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])(eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})`)},
}

// Text returns s with every credential it recognises replaced by [redacted <kind>].
func Text(s string) string {
	if s == "" {
		return s
	}
	s = envValues(s)
	s = pem.ReplaceAllString(s, "${1}[redacted private key]${2}")
	s = urlCredentials(s)
	for _, p := range prefixes {
		s = replacePrefix(s, p)
	}
	s = bearers(s)
	s = plistValues(s)
	s = keyValues(s)
	return s
}

func redacted(kind string) string { return "[redacted " + kind + "]" }

func tokenChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func replacePrefix(s string, p prefix) string {
	matches := p.re.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		start, end := m[4], m[5]
		if end < len(s) && tokenChar(s[end]) {
			continue
		}
		if p.digit && !strings.ContainsAny(s[start:end], "0123456789") {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(redacted(p.kind))
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

func urlCredentials(s string) string {
	return replaceGroup(s, urlCredential, 2, func(v string) (string, bool) {
		if placeholder(v) {
			return "", false
		}
		return redacted("password"), true
	})
}

func bearers(s string) string {
	return replaceGroup(s, bearer, 2, func(v string) (string, bool) {
		if !strings.ContainsAny(v, "0123456789") || placeholder(v) {
			return "", false
		}
		return redacted("bearer token"), true
	})
}

func replaceGroup(s string, re *regexp.Regexp, group int, fix func(string) (string, bool)) string {
	matches := re.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		start, end := m[2*group], m[2*group+1]
		if start < 0 {
			continue
		}
		out, ok := fix(s[start:end])
		if !ok {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(out)
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// keyValues resumes a rejected match from the end of its key, so a pair inside another pair's value is still found.
func keyValues(s string) string {
	var b strings.Builder
	last, pos := 0, 0
	for pos < len(s) {
		m := keyValue.FindStringSubmatchIndex(s[pos:])
		if m == nil {
			break
		}
		group := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return s[pos+m[2*i] : pos+m[2*i+1]]
		}
		keyOpen, key, keyClose := group(2), group(3), group(4)
		valueOpen, value, valueClose := group(6), group(7), group(8)
		if !secretPair(keyOpen, key, keyClose, valueOpen, value, valueClose) {
			pos += m[7]
			continue
		}
		b.WriteString(s[last : pos+m[14]])
		b.WriteString(redacted("secret"))
		last = pos + m[15]
		pos = last
	}
	b.WriteString(s[last:])
	return b.String()
}

func secretPair(keyOpen, key, keyClose, valueOpen, value, valueClose string) bool {
	if keyOpen != keyClose || !secretKey(key) {
		return false
	}
	quoted := valueOpen != ""
	if quoted && valueOpen != valueClose {
		return false
	}
	if !quoted && (!strings.ContainsAny(value, "0123456789") || strings.ContainsAny(value, "()$") || selector.MatchString(value)) {
		return false
	}
	return secretValue(value)
}

func plistValues(s string) string {
	matches := plistKey.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		key, value := s[m[4]:m[5]], s[m[8]:m[9]]
		if !secretKey(key) || strings.ContainsAny(value, " \t\r\n") || !secretValue(value) {
			continue
		}
		b.WriteString(s[last:m[8]])
		b.WriteString(redacted("secret"))
		last = m[9]
	}
	b.WriteString(s[last:])
	return b.String()
}

// secretKey reports a key name that holds a credential, which "author" alone does not.
func secretKey(key string) bool {
	k := strings.ToLower(key)
	k = strings.NewReplacer("authoriz", "auth", "authoris", "auth").Replace(k)
	k = strings.ReplaceAll(k, "author", "")
	return keyName.MatchString(k)
}

// secretValue reports a value long and varied enough to be a credential rather than a word, number or path.
func secretValue(v string) bool {
	if len(v) < 12 || placeholder(v) || path(v) {
		return false
	}
	classes := 0
	for _, set := range []string{"abcdefghijklmnopqrstuvwxyz", "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "0123456789"} {
		if strings.ContainsAny(v, set) {
			classes++
		}
	}
	return classes >= 2
}

func path(v string) bool {
	return strings.HasPrefix(v, "/") || strings.HasPrefix(v, "~/") || strings.HasPrefix(v, "./") || strings.HasPrefix(v, "../")
}

func placeholder(v string) bool {
	l := strings.ToLower(v)
	if strings.HasPrefix(l, "$") || strings.HasPrefix(l, "<") || strings.HasPrefix(l, "[redacted") || strings.HasPrefix(l, "{{") {
		return true
	}
	if strings.Trim(l, "*x.-_•") == "" {
		return true
	}
	for _, word := range []string{"changeme", "redacted", "xxxx", "***", "placeholder", "your_", "your-"} {
		if strings.Contains(l, word) {
			return true
		}
	}
	return false
}

// envValues redacts the values of this process's own credential variables wherever they appear.
func envValues(s string) string {
	type secret struct{ name, value string }
	var secrets []secret
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || len(value) < 8 || !secretKey(name) || placeholder(value) || path(value) {
			continue
		}
		if !strings.ContainsAny(value, "0123456789") || !strings.ContainsFunc(value, letter) {
			continue
		}
		secrets = append(secrets, secret{name, value})
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i].value) > len(secrets[j].value) })
	for _, e := range secrets {
		s = strings.ReplaceAll(s, e.value, redacted(e.name))
	}
	return s
}

func letter(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }
