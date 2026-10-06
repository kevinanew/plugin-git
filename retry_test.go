package main

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Use a real git-http-backend, injecting failures only at the HTTP boundary
// so fetch and partial checkout still execute their actual Git operations.
func TestPluginRetriesGitHTTP(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	runRetryTestGit(t, "init", "-q", "-b", "main", source)
	if err := os.WriteFile(filepath.Join(source, "README"), []byte("retry fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runRetryTestGit(t, "-C", source, "add", "README")
	runRetryTestGit(t, "-C", source, "-c", "user.name=CI", "-c", "user.email=ci@example.invalid", "commit", "-q", "-m", "创建重试验收仓库")
	sha := strings.TrimSpace(runRetryTestGit(t, "-C", source, "rev-parse", "HEAD"))
	remote := filepath.Join(root, "remote.git")
	runRetryTestGit(t, "clone", "-q", "--bare", source, remote)
	runRetryTestGit(t, "--git-dir", remote, "config", "uploadpack.allowFilter", "true")
	runRetryTestGit(t, "--git-dir", remote, "config", "uploadpack.allowAnySHA1InWant", "true")
	gitExecPath := strings.TrimSpace(runRetryTestGit(t, "--exec-path"))
	backend := &cgi.Handler{
		Path: filepath.Join(gitExecPath, "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	cases := []struct {
		name        string
		failAt      int32
		statuses    []int
		retries     int
		wantFailure bool
		wantErrors  int32
	}{
		{"fetch recovers", 1, []int{503, 503}, 2, false, 2},
		{"partial checkout recovers", 2, []int{503, 503}, 2, false, 2},
		{"retry limit", 1, []int{503, 503, 503, 503}, 2, true, 3},
		{"authentication recovers", 1, []int{401}, 2, false, 1},
		{"forbidden recovers", 1, []int{403, 403}, 2, false, 2},
		{"permanent failure", 1, []int{403, 403, 403, 403}, 2, true, 3},
		{"retries disabled", 1, []int{503}, 0, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var posts, failures atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				username, password, ok := r.BasicAuth()
				if !ok || username != "ci" || password != "test-password" {
					w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
					http.Error(w, "authentication required", http.StatusUnauthorized)
					return
				}
				if r.Method == http.MethodPost {
					index := posts.Add(1) - tc.failAt
					if index >= 0 && int(index) < len(tc.statuses) {
						failures.Add(1)
						http.Error(w, "injected transport failure", tc.statuses[index])
						return
					}
				}
				backend.ServeHTTP(w, r)
			}))
			defer server.Close()
			home, workspace := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("NO_PROXY", "127.0.0.1,localhost")
			oldEnv := defaultEnvVars
			t.Cleanup(func() { defaultEnvVars = oldEnv })
			oldUmask := umask(0o22)
			t.Cleanup(func() { umask(oldUmask) })
			plugin := Plugin{
				Repo:     Repo{Clone: server.URL + "/remote.git"},
				Pipeline: Pipeline{Path: workspace, Commit: sha, Event: "push"},
				Netrc:    Netrc{Machine: "127.0.0.1", Login: "ci", Password: "test-password"},
				Config: Config{
					Home:          home,
					Branch:        "main",
					Depth:         1,
					filter:        "tree:0",
					SafeDirectory: workspace,
				},
				Backoff: Backoff{Attempts: tc.retries, Duration: time.Millisecond},
			}
			err := plugin.Exec()
			if (err != nil) != tc.wantFailure {
				t.Fatalf("Exec() error = %v, want failure %v", err, tc.wantFailure)
			}
			if got := failures.Load(); got != tc.wantErrors {
				t.Fatalf("HTTP failures = %d, want %d", got, tc.wantErrors)
			}
			if !tc.wantFailure {
				data, err := os.ReadFile(filepath.Join(workspace, "README"))
				if err != nil || string(data) != "retry fixture\n" {
					t.Fatalf("checkout content = %q, error = %v", data, err)
				}
			}
		})
	}
}

func runRetryTestGit(t *testing.T, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
