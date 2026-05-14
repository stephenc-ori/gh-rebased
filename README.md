# gh-rebased

> Your branch survived the squash. Now rebase it properly.

When a parent PR in your stack gets squash-merged, a plain `git rebase` doesn't know what GitHub knows. It replays commits that are already in the base — just wearing a different SHA — and chaos follows.

`gh-rebased` asks GitHub which commits got squashed, drops them before the rebase runs, and gets out of your way.

---

![your git log after a squash merge](https://i.programmerhumor.io/2025/12/eb99fe9fb08a75fad449dd1d394d00b6b5c1b0cf37704b2bdbb93cc9b5b0d960.png)

*your branch, post-squash, pre-rebased*

---

## Install

```sh
gh extension install stephenc-ori/gh-rebased
```

## Usage

From your stacked branch, after the parent PR has been squash-merged:

```sh
gh rebased
```

That's it. It fetches the target branch, detects squash-merged parent commits, and runs the rebase — dropping only what's already been incorporated upstream.

## How it works

1. Finds the open PR for your current branch and its target branch
2. Fetches `origin/<target>` and computes the divergence point
3. For each new commit on the target, asks GitHub which PR it came from
4. If one new commit maps to a PR with multiple original commits → squash merge detected
5. Drops those original commits via `GIT_SEQUENCE_EDITOR` before `git rebase -i` runs
6. Rebase merges and single-commit PRs are left alone — `git rebase` handles those via patch-ID matching

## See also

These tools take a broader approach to stacked PR workflows:

- **[git-machete](https://github.com/VirtusLab/git-machete)** — full branch relationship manager with rebase automation across the whole stack
- **[gh-stack](https://github.com/github/gh-stack)** — GitHub's official stacked PR extension; handles cascading rebases and PR creation
- **[gh-domino](https://github.com/134130/gh-domino)** — rebases stacked PRs like dominoes when one in the chain is merged

`gh-rebased` is narrower: it does one thing, on one branch, when you need it.

## License

MIT
