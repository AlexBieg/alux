package netfail

import "testing"

func TestTransientTellsNetworkFailuresFromAnswersAndRefusals(t *testing.T) {
	for _, c := range []struct {
		text string
		line string
	}{
		{"gh pr list --repo acme/app --head feat-w1 --state all --limit 1 --json number,url,state,isDraft,headRefOid: exit status 1\nerror connecting to api.github.com\ncheck your internet connection or https://githubstatus.com", "error connecting to api.github.com"},
		{"gh pr checks 7 --repo acme/app --json name,bucket,link: exit status 1\nPost \"https://api.github.com/graphql\": dial tcp: lookup api.github.com: no such host", "Post \"https://api.github.com/graphql\": dial tcp: lookup api.github.com: no such host"},
		{"gh run view 42 --repo acme/app --log-failed: exit status 1\nGet \"https://api.github.com/repos/acme/app/actions/runs/42\": net/http: TLS handshake timeout", "Get \"https://api.github.com/repos/acme/app/actions/runs/42\": net/http: TLS handshake timeout"},
		{"gh api graphql: exit status 1\nPost \"https://api.github.com/graphql\": read tcp 192.168.1.4:50122->140.82.112.6:443: read: connection reset by peer", "Post \"https://api.github.com/graphql\": read tcp 192.168.1.4:50122->140.82.112.6:443: read: connection reset by peer"},
		{"jj git push --bookmark feat-w1 in /ws/T-1: exit status 1\nssh: connect to host github.com port 22: Operation timed out\nfatal: Could not read from remote repository.", "ssh: connect to host github.com port 22: Operation timed out"},
		{"jj git push --bookmark feat-w1 in /ws/T-1: exit status 1\nssh: Could not resolve hostname github.com: nodename nor servname provided, or not known\nfatal: Could not read from remote repository.", "ssh: Could not resolve hostname github.com: nodename nor servname provided, or not known"},
		{"jj git push --bookmark feat-w1 in /ws/T-1: exit status 1\nfatal: unable to access 'https://github.com/acme/app.git/': Failed to connect to github.com port 443 after 3 ms: Connection refused", "fatal: unable to access 'https://github.com/acme/app.git/': Failed to connect to github.com port 443 after 3 ms: Connection refused"},
		{"error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com", "error connecting to api.github.com"},
		{"curl: (6) Could not resolve host: ci.example.com", "curl: (6) Could not resolve host: ci.example.com"},
		{"  Get \"https://api.github.com/graphql\": dial tcp: lookup api.github.com: i/o timeout  ", "Get \"https://api.github.com/graphql\": dial tcp: lookup api.github.com: i/o timeout"},
		{"jj git push --bookmark feat-w1 in /ws/T-1: exit status 1\nTo github.com:acme/app.git\n ! [rejected]        feat-w1 -> feat-w1 (non-fast-forward)", ""},
		{"jj git push --bookmark feat-w1 in /ws/T-1: exit status 1\nError: Failed to push some bookmarks\nHint: The following references unexpectedly moved on the remote:\n  refs/heads/feat-w1 (reason: stale info)", ""},
		{"jj git push --bookmark feat-w1 in /ws/T-1: exit status 1\ngit@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.", ""},
		{"gh pr list --repo acme/app --head feat-w1: exit status 1\nHTTP 401: Bad credentials (https://api.github.com/graphql)\nTry authenticating with:  gh auth login", ""},
		{"gh pr view 7 --repo acme/app: exit status 1\nGraphQL: Could not resolve to a PullRequest with the number of 7. (repository.pullRequest)", ""},
		{"build\tpending\t0\thttps://github.com/acme/app/actions/runs/1\nlint\tpass\t12s\thttps://github.com/acme/app/actions/runs/2", ""},
		{"no checks reported on the 'feat-w1' branch", ""},
		{"curl: (7) Failed to connect to localhost port 8080 after 0 ms: Couldn't connect to server: Connection refused", ""},
		{"Get \"http://127.0.0.1:9000/healthcheck\": dial tcp 127.0.0.1:9000: connect: connection refused", ""},
		{"", ""},
	} {
		if got := Line(c.text); got != c.line {
			t.Errorf("Line(%q) = %q, want %q", c.text, got, c.line)
		}
		if got, want := Transient(c.text), c.line != ""; got != want {
			t.Errorf("Transient(%q) = %v, want %v", c.text, got, want)
		}
	}
}
