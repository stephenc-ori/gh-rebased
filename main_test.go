package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
)

// emptyTreeSHA is the SHA of the empty git tree object — always present in any repo.
const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

type testEnv struct {
	t        *testing.T
	dir      string
	owner    string
	testRepo string
	client   *api.RESTClient
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	repo, err := repository.Current()
	if err != nil {
		t.Fatalf("repository.Current: %v", err)
	}
	client, err := api.DefaultRESTClient()
	if err != nil {
		t.Fatalf("api.DefaultRESTClient: %v", err)
	}

	owner := repo.Owner
	testRepo := repo.Name + "-test"
	dir := t.TempDir()

	// Use HTTPS with token when GH_TOKEN is set (CI), otherwise SSH (local).
	cloneURL := fmt.Sprintf("git@github.com:%s/%s.git", owner, testRepo)
	if tok := os.Getenv("GH_TOKEN"); tok != "" {
		cloneURL = fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git", tok, owner, testRepo)
	}
	if out, err := exec.Command("git", "clone", cloneURL, dir).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}

	// Ensure git user identity is set for commits made by the test.
	exec.Command("git", "-C", dir, "config", "user.email", "ci@gh-rebased.test").Run() //nolint
	exec.Command("git", "-C", dir, "config", "user.name", "gh-rebased CI").Run()       //nolint

	e := &testEnv{t: t, dir: dir, owner: owner, testRepo: testRepo, client: client}
	e.closeAllOpenPRs()
	e.deleteRemoteBranchesExceptMain()
	e.resetMain()
	return e
}

func (e *testEnv) git(args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = e.dir
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		e.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	return strings.TrimSpace(string(out))
}

func (e *testEnv) gitQuiet(args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = e.dir
	cmd.Run() // ignore error — used for cleanup
}

func (e *testEnv) resetMain() {
	e.t.Helper()
	// Create a new root commit with an empty tree, giving a clean history.
	commitSHA := e.git("commit-tree", emptyTreeSHA, "-m", "chore: test baseline")
	if out, err := exec.Command("git", "-C", e.dir, "push", "--force",
		"origin", commitSHA+":refs/heads/main").CombinedOutput(); err != nil {
		e.t.Fatalf("force-push reset main: %v\n%s", err, out)
	}
	e.git("checkout", "-B", "main", commitSHA)
	e.git("branch", "--set-upstream-to=origin/main", "main")
}

func (e *testEnv) closeAllOpenPRs() {
	var prs []pr
	if err := e.client.Get(
		fmt.Sprintf("repos/%s/%s/pulls?state=open&per_page=100", e.owner, e.testRepo),
		&prs); err != nil {
		return
	}
	body, _ := json.Marshal(map[string]string{"state": "closed"})
	for _, p := range prs {
		e.client.Patch( //nolint
			fmt.Sprintf("repos/%s/%s/pulls/%d", e.owner, e.testRepo, p.Number),
			bytes.NewReader(body), nil)
	}
}

func (e *testEnv) deleteRemoteBranchesExceptMain() {
	out, err := exec.Command("git", "-C", e.dir, "ls-remote", "--heads", "origin").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		branch := strings.TrimPrefix(parts[1], "refs/heads/")
		if branch != "main" {
			exec.Command("git", "-C", e.dir, "push", "origin", "--delete", branch).Run() //nolint
		}
	}
}

func (e *testEnv) checkout(branch string) {
	e.t.Helper()
	e.git("checkout", branch)
}

func (e *testEnv) newBranch(name string) {
	e.t.Helper()
	e.git("checkout", "-b", name)
}

func (e *testEnv) commit(file, content, msg string) {
	e.t.Helper()
	if err := os.WriteFile(e.dir+"/"+file, []byte(content), 0644); err != nil {
		e.t.Fatalf("WriteFile %s: %v", file, err)
	}
	e.git("add", file)
	e.git("commit", "-m", msg)
}

func (e *testEnv) push(branch string) {
	e.t.Helper()
	e.git("push", "-u", "origin", branch)
}

func (e *testEnv) forcePush(branch string) {
	e.t.Helper()
	e.git("push", "--force-with-lease", "origin", branch)
}

func (e *testEnv) openPR(branch, title string) int {
	e.t.Helper()
	body, _ := json.Marshal(map[string]string{
		"title": title, "head": branch, "base": "main", "body": "",
	})
	var result struct {
		Number int `json:"number"`
	}
	if err := e.client.Post(
		fmt.Sprintf("repos/%s/%s/pulls", e.owner, e.testRepo),
		bytes.NewReader(body), &result); err != nil {
		e.t.Fatalf("openPR %s: %v", branch, err)
	}
	return result.Number
}

func (e *testEnv) squashMergePR(n int) {
	e.t.Helper()
	body, _ := json.Marshal(map[string]string{"merge_method": "squash"})
	if err := e.client.Put(
		fmt.Sprintf("repos/%s/%s/pulls/%d/merge", e.owner, e.testRepo, n),
		bytes.NewReader(body), nil); err != nil {
		e.t.Fatalf("squashMergePR #%d: %v", n, err)
	}
}

func (e *testEnv) runRebaser(branch string) error {
	r := &rebaser{
		owner:  e.owner,
		repo:   e.testRepo,
		dir:    e.dir,
		client: e.client,
		stderr: os.Stderr,
		stdin:  strings.NewReader(""),
		stdout: io.Discard,
	}
	return r.run(branch)
}

// commitMessages returns the subject lines of commits on HEAD above origin/main.
func (e *testEnv) commitMessages() []string {
	e.t.Helper()
	e.gitQuiet("fetch", "origin", "main")
	out := e.git("log", "--format=%s", "origin/main..HEAD")
	if out == "" {
		return nil
	}
	var msgs []string
	for _, l := range strings.Split(out, "\n") {
		if l != "" {
			msgs = append(msgs, l)
		}
	}
	return msgs
}

// --- tests ---

func TestNoOp_AlreadyUpToDate(t *testing.T) {
	e := newTestEnv(t)

	e.newBranch("feature-a")
	e.commit("a1.txt", "a1", "feat: a1")
	e.commit("a2.txt", "a2", "feat: a2")
	e.push("feature-a")
	e.openPR("feature-a", "feat: feature a")

	// No merges yet — should be no-op
	if err := e.runRebaser("feature-a"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := e.commitMessages()
	if len(msgs) != 2 {
		t.Errorf("expected 2 commits, got %d: %v", len(msgs), msgs)
	}
}

func TestSquashMergedParent_MultiCommit(t *testing.T) {
	e := newTestEnv(t)

	// feature-a: 2 commits targeting main
	e.newBranch("feature-a")
	e.commit("a1.txt", "a1", "feat: a1")
	e.commit("a2.txt", "a2", "feat: a2")
	e.push("feature-a")
	pr1 := e.openPR("feature-a", "feat: feature a")

	// feature-b: stacked on feature-a, 2 own commits, also targets main
	e.newBranch("feature-b")
	e.commit("b1.txt", "b1", "feat: b1")
	e.commit("b2.txt", "b2", "feat: b2")
	e.push("feature-b")
	e.openPR("feature-b", "feat: feature b")

	e.squashMergePR(pr1)

	e.checkout("feature-b")
	if err := e.runRebaser("feature-b"); err != nil {
		t.Fatalf("rebase failed: %v", err)
	}

	msgs := e.commitMessages()
	if len(msgs) != 2 {
		t.Errorf("expected 2 commits (b1, b2), got %d: %v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if strings.Contains(m, ": a") {
			t.Errorf("feature-a commit survived rebase: %q", m)
		}
	}
}

func TestSquashMergedParent_SingleCommit(t *testing.T) {
	e := newTestEnv(t)

	// feature-a: 1 commit — squash of single commit has same patch-ID as original,
	// so git rebase handles it automatically without explicit drops.
	e.newBranch("feature-a")
	e.commit("a1.txt", "a1", "feat: a1")
	e.push("feature-a")
	pr1 := e.openPR("feature-a", "feat: feature a")

	e.newBranch("feature-b")
	e.commit("b1.txt", "b1", "feat: b1")
	e.commit("b2.txt", "b2", "feat: b2")
	e.push("feature-b")
	e.openPR("feature-b", "feat: feature b")

	e.squashMergePR(pr1)

	e.checkout("feature-b")
	if err := e.runRebaser("feature-b"); err != nil {
		t.Fatalf("rebase failed: %v", err)
	}

	msgs := e.commitMessages()
	if len(msgs) != 2 {
		t.Errorf("expected 2 commits (b1, b2), got %d: %v", len(msgs), msgs)
	}
}

func TestPatchIDMatching_AfterPriorRebase(t *testing.T) {
	e := newTestEnv(t)

	// Build 4-layer stack
	e.newBranch("feature-a")
	e.commit("a1.txt", "a1", "feat: a1")
	e.commit("a2.txt", "a2", "feat: a2")
	e.push("feature-a")
	pr1 := e.openPR("feature-a", "feat: a")

	e.newBranch("feature-b")
	e.commit("b1.txt", "b1", "feat: b1")
	e.commit("b2.txt", "b2", "feat: b2")
	e.push("feature-b")
	pr2 := e.openPR("feature-b", "feat: b")

	e.newBranch("feature-c")
	e.commit("c1.txt", "c1", "feat: c1")
	e.push("feature-c")
	pr3 := e.openPR("feature-c", "feat: c")

	e.newBranch("feature-d")
	e.commit("d1.txt", "d1", "feat: d1")
	e.commit("d2.txt", "d2", "feat: d2")
	e.push("feature-d")
	e.openPR("feature-d", "feat: d")

	// Phase 1: squash-merge feature-a, rebase feature-d (SHA match)
	e.squashMergePR(pr1)
	e.checkout("feature-d")
	if err := e.runRebaser("feature-d"); err != nil {
		t.Fatalf("first rebase: %v", err)
	}
	e.forcePush("feature-d")

	if msgs := e.commitMessages(); len(msgs) != 5 {
		t.Errorf("after first rebase: expected 5 commits, got %d: %v", len(msgs), msgs)
	}

	// Phase 2: squash-merge feature-b and feature-c
	e.squashMergePR(pr2)
	e.squashMergePR(pr3)

	// Phase 3: rebase feature-d again — must use patch-ID matching (SHAs changed)
	if err := e.runRebaser("feature-d"); err != nil {
		t.Fatalf("second rebase: %v", err)
	}

	msgs := e.commitMessages()
	if len(msgs) != 2 {
		t.Errorf("after second rebase: expected 2 commits (d1, d2), got %d: %v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if !strings.Contains(m, ": d") {
			t.Errorf("unexpected commit in final history: %q", m)
		}
	}
}
