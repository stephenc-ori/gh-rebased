package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
)

type pr struct {
	Number int `json:"number"`
	Base   struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type prCommit struct {
	SHA string `json:"sha"`
}

// rebaser holds context for one rebase operation.
type rebaser struct {
	owner  string
	repo   string
	dir    string          // git working directory; "" = cwd
	client *api.RESTClient
	stderr io.Writer       // status message output
	stdin  io.Reader       // subprocess stdin
	stdout io.Writer       // subprocess stdout
}

func (r *rebaser) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func parseSHASet(output string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, line := range strings.Split(output, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			set[s] = struct{}{}
		}
	}
	return set
}

func (r *rebaser) patchID(sha string) string {
	diffCmd := exec.Command("git", "diff-tree", "-p", sha)
	diffCmd.Dir = r.dir
	diff, err := diffCmd.Output()
	if err != nil || len(diff) == 0 {
		return ""
	}
	pidCmd := exec.Command("git", "patch-id", "--stable")
	pidCmd.Dir = r.dir
	pidCmd.Stdin = bytes.NewReader(diff)
	out, err := pidCmd.Output()
	if err != nil {
		return ""
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) < 1 {
		return ""
	}
	return parts[0]
}

func (r *rebaser) patchIDDropSet(parentOriginalSHAs, branchSHAs map[string]struct{}) map[string]struct{} {
	parentPIDs := make(map[string]struct{}, len(parentOriginalSHAs))
	for sha := range parentOriginalSHAs {
		if pid := r.patchID(sha); pid != "" {
			parentPIDs[pid] = struct{}{}
		}
	}
	if len(parentPIDs) == 0 {
		return nil
	}
	drops := map[string]struct{}{}
	for sha := range branchSHAs {
		if pid := r.patchID(sha); pid != "" {
			if _, match := parentPIDs[pid]; match {
				drops[sha] = struct{}{}
			}
		}
	}
	return drops
}

func (r *rebaser) run(branch string) error {
	var prs []pr
	if err := r.client.Get(fmt.Sprintf("repos/%s/%s/pulls?head=%s:%s&state=open", r.owner, r.repo, r.owner, branch), &prs); err != nil {
		return fmt.Errorf("listing PRs: %w", err)
	}
	if len(prs) == 0 {
		fmt.Fprintf(r.stderr, "no open PR found for branch %q\n", branch)
		return nil
	}
	base := prs[0].Base.Ref

	fmt.Fprintf(r.stderr, "Fetching origin/%s...\n", base)
	if _, err := r.git("fetch", "origin", base); err != nil {
		return err
	}

	mergeBase, err := r.git("merge-base", "HEAD", "origin/"+base)
	if err != nil {
		return fmt.Errorf("branch is not based on origin/%s", base)
	}

	newBaseLog, err := r.git("log", "--format=%H %P", mergeBase+"..origin/"+base)
	if err != nil {
		return err
	}

	type commitInfo struct{ parentCount int }
	newBaseCommits := map[string]commitInfo{}
	for _, line := range strings.Split(newBaseLog, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		newBaseCommits[fields[0]] = commitInfo{parentCount: len(fields) - 1}
	}

	if len(newBaseCommits) == 0 {
		fmt.Fprintf(r.stderr, "Already up to date with origin/%s.\n", base)
		return nil
	}

	branchOut, err := r.git("log", "--format=%H", mergeBase+"..HEAD")
	if err != nil {
		return err
	}
	branchSHAs := parseSHASet(branchOut)
	if len(branchSHAs) == 0 {
		fmt.Fprintf(r.stderr, "No commits on branch since divergence from origin/%s.\n", base)
		return nil
	}

	fmt.Fprintln(r.stderr, "Finding squash-merged parent commits...")

	prToNewSHAs := map[int][]string{}
	for sha, info := range newBaseCommits {
		if info.parentCount != 1 {
			continue
		}
		var assocPRs []pr
		err := r.client.Get(fmt.Sprintf("repos/%s/%s/commits/%s/pulls", r.owner, r.repo, sha), &assocPRs)
		if err != nil {
			var httpErr *api.HTTPError
			if errors.As(err, &httpErr) && httpErr.StatusCode == 404 {
				continue
			}
			fmt.Fprintf(r.stderr, "  warning: could not get PRs for commit %.8s: %v\n", sha, err)
			continue
		}
		for _, p := range assocPRs {
			prToNewSHAs[p.Number] = append(prToNewSHAs[p.Number], sha)
		}
	}

	parentOriginalSHAs := map[string]struct{}{}
	for prNum, newSHAs := range prToNewSHAs {
		if len(newSHAs) != 1 {
			continue
		}
		var commits []prCommit
		if err := r.client.Get(fmt.Sprintf("repos/%s/%s/pulls/%d/commits", r.owner, r.repo, prNum), &commits); err != nil {
			fmt.Fprintf(r.stderr, "  warning: could not get commits for PR #%d: %v\n", prNum, err)
			continue
		}
		if len(commits) <= 1 {
			continue
		}
		fmt.Fprintf(r.stderr, "  PR #%d was squash-merged — dropping %d commits\n", prNum, len(commits))
		for _, c := range commits {
			parentOriginalSHAs[c.SHA] = struct{}{}
		}
	}

	dropSHAs := map[string]struct{}{}
	for sha := range parentOriginalSHAs {
		if _, onBranch := branchSHAs[sha]; onBranch {
			dropSHAs[sha] = struct{}{}
		}
	}
	if len(dropSHAs) == 0 && len(parentOriginalSHAs) > 0 {
		fmt.Fprintln(r.stderr, "  (matching by patch content — branch was previously rebased)")
		dropSHAs = r.patchIDDropSet(parentOriginalSHAs, branchSHAs)
	}

	if len(dropSHAs) == 0 {
		fmt.Fprintf(r.stderr, "No squash-merged parent commits found on branch. Running plain rebase onto origin/%s.\n", base)
		cmd := exec.Command("git", "rebase", "origin/"+base)
		cmd.Dir = r.dir
		cmd.Stdin, cmd.Stdout, cmd.Stderr = r.stdin, r.stdout, r.stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("rebase failed")
		}
		return nil
	}

	shaList := make([]string, 0, len(dropSHAs))
	for sha := range dropSHAs {
		shaList = append(shaList, sha)
	}
	script := fmt.Sprintf(`#!/bin/sh
perl -i -pe 'BEGIN{%%d=map{$_=>1}qw(%s)} s/^pick (\S+)/exists $d{$1} ? "drop $1" : "pick $1"/e' "$1"
`, strings.Join(shaList, " "))

	tmpFile, err := os.CreateTemp("", "gh-rebased-*.sh")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.WriteString(script); err != nil {
		return err
	}
	tmpFile.Close()
	if err := os.Chmod(tmpFile.Name(), 0700); err != nil {
		return err
	}

	fmt.Fprintf(r.stderr, "Rebasing %s onto origin/%s (dropping %d commits)...\n", branch, base, len(dropSHAs))

	cmd := exec.Command("git", "-c", "core.abbrev=40", "rebase", "-i", "origin/"+base)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_SEQUENCE_EDITOR="+tmpFile.Name())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.stdin, r.stdout, r.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rebase failed")
	}

	fmt.Fprintln(r.stderr, "Successfully rebased.")
	return nil
}

func main() {
	repo, err := repository.Current()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	client, err := api.DefaultRESTClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	r := &rebaser{
		owner:  repo.Owner,
		repo:   repo.Name,
		dir:    "",
		client: client,
		stderr: os.Stderr,
		stdin:  os.Stdin,
		stdout: os.Stdout,
	}

	branch, err := r.git("branch", "--show-current")
	if err != nil || branch == "" {
		fmt.Fprintln(os.Stderr, "error: detached HEAD or unable to determine current branch")
		os.Exit(1)
	}

	if err := r.run(branch); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
