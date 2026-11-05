# Install and use split-gh

The examples use `ai` for the agent qube and `gh-auth` for the authenticated
qube. Each repository has the same path in both, e.g. `~/lnd`. Replace the
names with yours. Run each command in the qube its section names.

What split-gh does:

- **Reads GitHub.** In `ai`, `gh` is a shim. `split-gh serve` in gh-auth runs
  the real gh with a read-only token, for allowlisted commands and
  repositories.
- **Fetches from GitHub.** In `ai`, `git clone` and `git fetch` of
  `https://github.com/...` go through `split-gh serve`, which adds the token.
  So private repositories work, and pushes are refused.
- **Hands over commits as a patch file.** In `ai`, `split-gh send` moves a
  patch file to gh-auth with `qvm-move`, which you confirm in dom0. In gh-auth,
  you review it with `split-gh diff`, take it with `split-gh apply`, and push
  yourself.

Nothing that runs in `ai` can change gh-auth or GitHub. `split-gh serve` only
reads, and only while you run it. A patch only arrives when you accept the
`qvm-move`.

## gh-auth

Get the source into gh-auth, e.g. as a bundle: in `ai`, run
`git bundle create /tmp/split-gh.bundle --all && qvm-move /tmp/split-gh.bundle`,
then in gh-auth run `git clone ~/QubesIncoming/ai/split-gh.bundle split-gh`.
Review the code there, not in `ai`. **Do not copy a binary from ai.** Then
build and install it:

```sh
CGO_ENABLED=0 go build -trimpath -o split-gh ./cmd/split-gh
sudo install -m 755 split-gh /usr/local/bin/split-gh
install -d -m 700 ~/.config/split-gh
```

Create `~/.config/split-gh/config.json`:

```json
{
  "qubes": {
    "ai": {
      "port": 8401,
      "password_file": "~/.config/split-gh/ai.password",
      "read_repos": ["example/*"],
      "local_repos": ["lnd"]
    }
  },
  "read_token_file": "~/.config/split-gh/read-token"
}
```

- `port` is where `split-gh serve` answers `ai`, on 127.0.0.1 in gh-auth. The
  dom0 policy lets only `ai` connect to it. Give each agent qube its own port:
  that is how serve tells them apart.
- `password_file` holds the password `ai` sends with every request. The
  policy already decides who can connect; the password is a second lock, in
  case the policy is ever wrong.
- `read_repos` are the GitHub repositories `ai` may read, with `gh` and with
  `git fetch`, as `OWNER/REPO` patterns.
- `local_repos` are the repositories in gh-auth, relative to your home
  directory, that `ai` may send patches for.

Make the password and send a copy to `ai`:

```sh
head -c 24 /dev/urandom | base64 > ~/.config/split-gh/ai.password
chmod 600 ~/.config/split-gh/ai.password
qvm-copy-to-vm ai ~/.config/split-gh/ai.password
```

Put a GitHub token in `~/.config/split-gh/read-token` and run `chmod 600` on
it. Prefer a fine-grained read-only token limited to the repositories you
allow, with Contents, Metadata, Issues, Pull requests and Actions read
permissions. Contents covers `git fetch` too. serve runs gh with a temporary
home and config, the token, and prompting disabled; it doesn't use your `gh`
aliases or extensions.

`split-gh apply` signs with `git am --gpg-sign`, so it uses the signing setup
in your git config, the same one that signs your own commits. git am runs in
the worktree, among the agent's files, so apply refuses a signing program
given as a relative path, such as `gpg.program=./sign`. A program name, such
as `gpg2`, or an absolute path is fine.

## dom0

Create `/etc/qubes/policy.d/30-split-gh.policy`, with your qube names:

```text
qubes.ConnectTCP +8401 ai gh-auth allow
qubes.ConnectTCP +8401 @anyvm @anyvm deny
```

`qubes.ConnectTCP` connects `ai` to port 8401 on 127.0.0.1 in gh-auth. In
gh-auth, `/etc/qubes-rpc/qubes.ConnectTCP` should be a symlink to
`/dev/tcp/127.0.0.1`: then qrexec connects to the port and starts nothing,
and while serve isn't running the connection is refused. Nothing in gh-auth
listens on a network.

Leave `qubes.Filecopy` from `ai` at `ask`, the default: that dialog is where
you confirm each patch.

## ai

Build the client from source and install it, with `gh` as a symlink, earlier
on PATH than any real `gh`:

```sh
mkdir -p ~/.local/bin
CGO_ENABLED=0 go build -trimpath -o ~/.local/bin/split-gh ./cmd/split-gh
ln -sfn split-gh ~/.local/bin/gh
export PATH="$HOME/.local/bin:$PATH"
```

Add the PATH setting to the shell's startup file, and check that `command -v gh`
prints the symlink. Then put the password in place:

```sh
install -d -m 700 ~/.config/split-gh
mv ~/QubesIncoming/gh-auth/ai.password ~/.config/split-gh/password
```

Forward port 8401 to gh-auth. Add this line to `/rw/config/rc.local`, so it
runs at boot, and run it once now:

```sh
qvm-connect-tcp 8401:gh-auth:8401
```

It starts `socat`, which listens on all of `ai`'s addresses. Qubes' firewall
keeps other machines and qubes from reaching them, and serve wants the
password anyway. For a second agent qube with port 8402 in gh-auth, run
`qvm-connect-tcp 8401:gh-auth:8402` there: the local port stays 8401.

Make git fetch from GitHub through gh-auth:

```sh
git config --global url.http://127.0.0.1:8401/.insteadOf https://github.com/
git config --global --add url.http://127.0.0.1:8401/.insteadOf git@github.com:
git config --global --add url.http://127.0.0.1:8401/.insteadOf ssh://git@github.com/
git config --global http.http://127.0.0.1:8401/.extraHeader \
  "Authorization: Bearer $(cat ~/.config/split-gh/password)"
```

`gh` with no arguments tells agents what it is and what they can do, and so
does every refusal. For more, put the instructions in `docs/agents.md` where
your agents read them, e.g. in `~/.claude/CLAUDE.md`.

## Running serve

In gh-auth, in a terminal:

```sh
split-gh serve
```

It prints a line for each request: the gh command line and how it ended, each
git fetch, and each refusal, such as a push. Ctrl-C stops it and ends the
requests in flight. While it isn't running, `gh` in `ai` says to ask you to
start it, and `git fetch` fails.

## Sending commits

The agent commits in a worktree, e.g. `~/lnd/fix-foo` on branch `fix-foo`, and
runs `split-gh send` there. You confirm the `qvm-move` in dom0, and it prints:

```text
split-gh diff ~/QubesIncoming/ai/lnd-fix-foo-1a2b3c4.patch
split-gh apply ~/QubesIncoming/ai/lnd-fix-foo-1a2b3c4.patch
```

Run them in gh-auth:

- `split-gh diff` shows where the patch goes and what apply would do. Below
  that, in `less -R`, is `diff -u --color=always` of the branch as it is now,
  formatted as patches, against the new patch. So you see what changed since
  the last version, including anything you changed in gh-auth. Control and
  bidirectional characters show as `<U+...>`, so a patch can't hide lines.
- `split-gh apply` updates the worktree at the same path, `~/lnd/fix-foo`,
  creating it and the branch if needed. Commits that didn't change are kept,
  so a series that only grew is a fast-forward. It signs the new commits and
  checks that they reproduce the patch exactly, then prints the push command.
  It refuses if the worktree has uncommitted changes. It also refuses to drop
  commits it didn't put on the branch, such as your own edits, unless you pass
  `--force`. No git hook or filter runs during apply, yours included: with a
  relative `core.hooksPath`, or a filter configured as a relative path, git
  would run them from the worktree, which holds the agent's files.

The worktree holds the agent's files. Read them there, but don't run
anything that uses them, such as a build, tests or `go generate`; do that
in `ai`. Git counts too: if your config gives `core.hooksPath` or a filter
as a relative path, git commands in the worktree run the agent's files.
That is why apply's own git commands run no hooks or filters, and why the
push command it prints runs from the main checkout. The main checkout's hooks
stay yours: apply refuses a worktree where git there looks for them, such as
`~/lnd/.githooks` when `core.hooksPath` is `.githooks`.

What send sends: the commits on the current branch that no remote has, so
`git rev-list HEAD --not --remotes`; or the ones after `--base COMMIT`. Their
parent must exist in gh-auth's repository. If apply says it doesn't, run
`git fetch` there. Send refuses:

- the main checkout, because gh-auth mirrors worktrees;
- merges and empty commits;
- paths and branch names with characters other than letters, digits and `._+-/`;
- a commit message that `git am` would change, such as one with a `---` line;
- a patch over 64 MiB, which apply wouldn't take.

If you commit on the branch in gh-auth, the agent sees your commits once you
push them, and should build on them. Until then, a patch from the agent drops
them, and apply refuses it unless you pass `--force`.

## Limits and the audit log

- gh: 60 requests a minute, 8 at once, and 60 s of gh for each.
- git: 120 requests a minute, 4 at once, 30 minutes each, and a minute
  without data ends one. A fetch takes two to four requests.
- Any request: a client that stops reading for 30 s, or takes longer than
  that to send its request, is cut off, so it can't hold a slot.

serve writes to `~/.local/state/split-gh/log.jsonl` in gh-auth. Requests over
the rate or concurrency limit aren't logged, so they can't fill the disk.
Every other request is logged before gh runs or GitHub is asked, and refused
if it can't be. When the log is over 100 MB, serve refuses requests until you
move it aside.

## What serve allows

- Searches require an allowed `--repo` or a fully allowed `--owner`. Search
  text and filters can't contain extra scope qualifiers, or the boolean
  operators and grouping that could widen that scope.
- REST paths under `repos/OWNER/REPO` are checked against the qube's
  repositories even outside a checkout, and limited to read routes: pulls,
  issues, commits, contents, git, branches, tags, releases, labels,
  milestones, compare, check-runs, check-suites, statuses and
  actions/{runs,jobs,workflows}. Paths can't contain `:`. Ordinary query
  strings are accepted, but method-override parameters are refused. The exact
  paths `meta`, `zen`, `emojis` and `rate_limit` are also allowed.
- Agent-written `gh api graphql` is refused. `gh pr threads N` runs one fixed
  GraphQL query and prints one JSON object per review thread.
- `--jq` runs in `ai` with the `jq` executable, because gh's jq can read
  environment variables, including the token. Install `jq` in `ai` if needed.
- `gh run view` without a run or job ID shows the newest run on the checkout's
  branch, because serve can't prompt.
- The shim finds the repository from `remote.<name>.gh-resolved`, then the
  `upstream` and `origin` remotes. `GH_REPO` overrides that, and `-R OWNER/REPO`
  picks a repository explicitly.

- `git` gets only the two requests of a fetch, for repositories in
  `read_repos`, and serve adds the token in gh-auth. A push is refused before
  it reaches GitHub, with a message that git shows as `remote:` lines. serve
  passes on only the headers git's protocol needs, and doesn't follow
  redirects.

## Verify

From a checkout in `ai` of an allowed private repository:

```sh
gh pr view 123 --comments
gh api repos/example/private/pulls/123/comments
gh run view --log-failed
gh pr threads 123
git ls-remote https://github.com/example/private
```

The full acceptance test runs from the split-gh source in `ai`, after setup:

```sh
SPLIT_GH_E2E_REPO=example/private \
SPLIT_GH_E2E_PR=123 \
SPLIT_GH_E2E_CHECKOUT="$HOME/path/to/checkout" \
go test ./cmd/split-gh -run '^TestQubesPrivateRead$' -count=1 -v
```

It checks the repository is private and runs the four reads. It also sends
hand-made write and GraphQL requests to serve to check they're refused, runs
`git ls-remote`, and checks that a `git push --dry-run` is refused.
The checkout's branch needs a completed workflow run. For the matrix of every
read command, also set `SPLIT_GH_E2E_ISSUE`, `SPLIT_GH_E2E_RUN`,
`SPLIT_GH_E2E_WORKFLOW` and `SPLIT_GH_E2E_RELEASE` to an issue, an Actions run,
a workflow file and a release tag in the same repository, then run
`-run '^TestQubesAllReadCommands$'`. The pull request needs passing checks.
