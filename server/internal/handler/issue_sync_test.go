package handler

import "testing"

func TestGithubRepoKeyFromURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"api_repos_url", "https://api.github.com/repos/acme/widget", "acme/widget"},
		{"html_url_with_path", "https://github.com/acme/widget/issues/42", "acme/widget"},
		{"html_url_trailing_slash", "https://github.com/Acme/Widget/", "acme/widget"},
		{"git_suffix", "https://github.com/acme/widget.git", "acme/widget"},
		{"empty", "", ""},
		{"no_repo_segment", "https://github.com/acme", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := githubRepoKeyFromURL(c.url); got != c.want {
				t.Fatalf("githubRepoKeyFromURL(%q) = %q, want %q", c.url, got, c.want)
			}
		})
	}
}

func TestGithubRepoKeyFromPayload(t *testing.T) {
	t.Run("nil_repo", func(t *testing.T) {
		if got := githubRepoKeyFromPayload(nil); got != "" {
			t.Fatalf("expected empty for nil repo, got %q", got)
		}
	})
	t.Run("full_name", func(t *testing.T) {
		repo := &struct {
			FullName string `json:"full_name"`
		}{FullName: "Acme/Widget"}
		if got := githubRepoKeyFromPayload(repo); got != "acme/widget" {
			t.Fatalf("expected acme/widget, got %q", got)
		}
	})
	t.Run("empty_full_name", func(t *testing.T) {
		repo := &struct {
			FullName string `json:"full_name"`
		}{FullName: ""}
		if got := githubRepoKeyFromPayload(repo); got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})
}

func TestIsGitHubAppLogin(t *testing.T) {
	if !isGitHubAppLogin("multica-app[bot]") {
		t.Fatal("expected bot login to be detected")
	}
	if isGitHubAppLogin("human-user") {
		t.Fatal("human logins must not be flagged as bots")
	}
}
