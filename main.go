package main

import (
	"bytes"
	"errors"
	"fmt"
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

func gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
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

// patchID returns the git patch-id for a commit, or "" if unavailable.
// Patch IDs are stable across rebases: same diff content → same patch ID,
// even when the commit SHA changes.
func patchID(sha string) string {
	diff, err := exec.Command("git", "diff-tree", "-p", sha).Output()
	if err != nil || len(diff) == 0 {
		return ""
	}
	cmd := exec.Command("git", "patch-id", "--stable")
	cmd.Stdin = bytes.NewReader(diff)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	// output: "<patch-id> <sha>\n"
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) < 1 {
		return ""
	}
	return parts[0]
}

// patchIDDropSet finds branch commits whose patch content matches any of the
// parent original commits, even when SHAs differ due to a prior rebase.
func patchIDDropSet(parentOriginalSHAs, branchSHAs map[string]struct{}) map[string]struct{} {
	parentPatchIDs := make(map[string]struct{}, len(parentOriginalSHAs))
	for sha := range parentOriginalSHAs {
		if pid := patchID(sha); pid != "" {
			parentPatchIDs[pid] = struct{}{}
		}
	}
	if len(parentPatchIDs) == 0 {
		return nil
	}
	drops := map[string]struct{}{}
	for sha := range branchSHAs {
		if pid := patchID(sha); pid != "" {
			if _, match := parentPatchIDs[pid]; match {
				drops[sha] = struct{}{}
			}
		}
	}
	return drops
}

func main() {
	repo, err := repository.Current()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	owner, repoName := repo.Owner, repo.Name

	branch, err := gitOutput("branch", "--show-current")
	if err != nil || branch == "" {
		fmt.Fprintln(os.Stderr, "error: detached HEAD or unable to determine current branch")
		os.Exit(1)
	}

	client, err := api.DefaultRESTClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	var prs []pr
	if err := client.Get(fmt.Sprintf("repos/%s/%s/pulls?head=%s:%s&state=open", owner, repoName, owner, branch), &prs); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if len(prs) == 0 {
		fmt.Fprintf(os.Stderr, "no open PR found for branch %q\n", branch)
		os.Exit(0)
	}
	base := prs[0].Base.Ref

	fmt.Fprintf(os.Stderr, "Fetching origin/%s...\n", base)
	if _, err := gitOutput("fetch", "origin", base); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	mergeBase, err := gitOutput("merge-base", "HEAD", "origin/"+base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: branch is not based on origin/%s\n", base)
		os.Exit(1)
	}

	newBaseLog, err := gitOutput("log", "--format=%H %P", mergeBase+"..origin/"+base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
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
		fmt.Fprintf(os.Stderr, "Already up to date with origin/%s.\n", base)
		os.Exit(0)
	}

	branchOut, err := gitOutput("log", "--format=%H", mergeBase+"..HEAD")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	branchSHAs := parseSHASet(branchOut)
	if len(branchSHAs) == 0 {
		fmt.Fprintf(os.Stderr, "No commits on branch since divergence from origin/%s.\n", base)
		os.Exit(0)
	}

	// Find squash-merged parent commits
	fmt.Fprintln(os.Stderr, "Finding squash-merged parent commits...")

	prToNewSHAs := map[int][]string{}
	for sha, info := range newBaseCommits {
		if info.parentCount != 1 {
			continue
		}
		var assocPRs []pr
		err := client.Get(fmt.Sprintf("repos/%s/%s/commits/%s/pulls", owner, repoName, sha), &assocPRs)
		if err != nil {
			var httpErr *api.HTTPError
			if errors.As(err, &httpErr) && httpErr.StatusCode == 404 {
				continue
			}
			fmt.Fprintf(os.Stderr, "  warning: could not get PRs for commit %.8s: %v\n", sha, err)
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
		if err := client.Get(fmt.Sprintf("repos/%s/%s/pulls/%d/commits", owner, repoName, prNum), &commits); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not get commits for PR #%d: %v\n", prNum, err)
			continue
		}
		if len(commits) <= 1 {
			// Single-commit PR: git rebase handles via patch-ID matching automatically
			continue
		}
		fmt.Fprintf(os.Stderr, "  PR #%d was squash-merged — dropping %d commits\n", prNum, len(commits))
		for _, c := range commits {
			parentOriginalSHAs[c.SHA] = struct{}{}
		}
	}

	// Build drop set: SHA match first, then patch-ID match for rebased commits
	dropSHAs := map[string]struct{}{}
	for sha := range parentOriginalSHAs {
		if _, onBranch := branchSHAs[sha]; onBranch {
			dropSHAs[sha] = struct{}{}
		}
	}
	if len(dropSHAs) == 0 && len(parentOriginalSHAs) > 0 {
		// Branch commits were replayed by a prior rebase — match by patch content
		fmt.Fprintln(os.Stderr, "  (matching by patch content — branch was previously rebased)")
		dropSHAs = patchIDDropSet(parentOriginalSHAs, branchSHAs)
	}

	if len(dropSHAs) == 0 {
		fmt.Fprintf(os.Stderr, "No squash-merged parent commits found on branch. Running plain rebase onto origin/%s.\n", base)
		cmd := exec.Command("git", "rebase", "origin/"+base)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			os.Exit(1)
		}
		return
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
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(script); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	tmpFile.Close()
	if err := os.Chmod(tmpFile.Name(), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Rebasing %s onto origin/%s (dropping %d commits)...\n", branch, base, len(dropSHAs))

	cmd := exec.Command("git", "-c", "core.abbrev=40", "rebase", "-i", "origin/"+base)
	cmd.Env = append(os.Environ(), "GIT_SEQUENCE_EDITOR="+tmpFile.Name())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "Successfully rebased.")
}
