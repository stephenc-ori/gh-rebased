# Development

## Running the tests

The integration tests hit the live GitHub API. They clone a companion repo,
reset its `main` branch, open and merge pull requests, and verify the rebase
output — so you need a few one-time setup steps before `go test` will work.

### 1. Create a test repository

Create a **private** repository named `<your-github-username>/gh-rebased-test`
(the tests derive the name from the current repo's owner and name):

```sh
gh repo create <your-username>/gh-rebased-test --private
```

No content needed — the tests reset `main` to a fresh empty commit before each run.

### 2. Create a fine-grained personal access token

Go to **GitHub → Settings → Developer settings → Personal access tokens →
Fine-grained tokens → Generate new token** and set:

| Field | Value |
|---|---|
| Repository access | Only `<your-username>/gh-rebased-test` |
| Contents | Read and write |
| Pull requests | Read and write |
| Metadata | Read (forced on) |

No other permissions are required.

### 3. Add the token as a repository secret

```sh
gh secret set GH_TEST_TOKEN \
  --repo <your-username>/gh-rebased \
  --body "<the token you just created>"
```

This secret is picked up by the CI workflow (`.github/workflows/test.yml`)
and also used locally when you set `GH_TOKEN` in your environment:

```sh
GH_TOKEN=<token> go test -v -timeout 10m
```

Without `GH_TOKEN` set, the tests fall back to SSH (`git@github.com:...`)
for cloning, which works if your local SSH key is configured for GitHub.

### 4. Run

```sh
go test -v -timeout 10m
```

Tests take ~90 seconds locally (four sequential GitHub API round-trips).
