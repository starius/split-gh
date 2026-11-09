# split-gh

Use GitHub from an agent qube without putting GitHub credentials there.
split-gh forwards allowed `gh` reads and GitHub Git fetches to a trusted qube.
Agents hand over commits as patches; you review, apply, and push them yourself.
Commands that write to GitHub are refused.

## Choose your qubes

This guide uses generic names. Substitute your own consistently.

| Qube | Role |
| --- | --- |
| `agent` | Runs agents, the `gh` shim, and Git checkouts; holds no GitHub token. |
| `github` | Runs the real `gh` and `split-gh serve`; holds a read token. |
| `source` | Optional separate trusted build, review, signing, and push qube. |
| `dom0` | Holds the qrexec policy and confirms file transfers. |

For two qubes, do the `source` work in `github`. For three, reads go from
`agent` to `github`, while patches go from `agent` to `source`.

Examples use GitHub repository `example/project`, fork `contributor/project`,
and main checkout `~/project`. Replace these placeholders before running commands.

## 1. Build and install

In your trusted build qube, review the source and build:

```sh
nix build .
sudo install -m 755 result/bin/split-gh /usr/local/bin/split-gh
```

Alternatively, with Go 1.22 or newer:

```sh
CGO_ENABLED=0 go build -trimpath -o split-gh ./cmd/split-gh
sudo install -m 755 split-gh /usr/local/bin/split-gh
```

Git 2.40 or newer is required in the agent and patch receiver. Install the real
GitHub CLI in `github`, and Git in each qube using repositories. Install `jq` in
`agent` for `gh --jq`. Older GitHub CLI versions may lack some read commands;
see [version notes and detailed installation](docs/install.md).

Copy the trusted build to `agent` and, if different, `github`. For a Nix build:

```sh
qvm-copy-to-vm agent result/bin/split-gh
qvm-copy-to-vm github result/bin/split-gh
```

In `github`, install the incoming binary, replacing `source` with the actual
sender's name:

```sh
sudo install -m 755 ~/QubesIncoming/source/split-gh /usr/local/bin/split-gh
```

Do not install a binary built in the agent qube into a trusted qube. You can
instead transfer source, review it, and build in each trusted destination.

## 2. Configure reads in `github`

```sh
install -d -m 700 ~/.config/split-gh
```

Create `~/.config/split-gh/config.json`:

```json
{
  "qubes": {
    "agent": {
      "port": 8401,
      "password_file": "~/.config/split-gh/agent.password",
      "read_repos": ["example/project", "contributor/project"],
      "local_repos": ["project"]
    }
  },
  "read_token_file": "~/.config/split-gh/read-token"
}
```

`read_repos` names GitHub repositories, including forks you need. `local_repos`
names main checkouts relative to your home directory and is used when receiving
patches. If patches go to a separate `source` qube, omit `local_repos` here and
configure it there in step 6. Repository patterns such as `example/*` are also
supported. Adding a repository here does not grant the GitHub token access to it.

To cover the same repository name across owners, use `"read_repos":
["*/project"]`. This matches `example/project`, `contributor/project`, and any
other owner's repository named `project`, case-insensitively. It matches by name,
not GitHub fork ancestry: unrelated repositories with that name are included,
and forks renamed to something else need another entry. The pattern applies to
both `gh` reads and Git fetches. It does not permit owner-wide searches or grant
the token additional access. Restart serve after changing the allowlist.

Create a client password and send it to `agent`:

```sh
head -c 24 /dev/urandom | base64 > ~/.config/split-gh/agent.password
chmod 600 ~/.config/split-gh/agent.password
qvm-copy-to-vm agent ~/.config/split-gh/agent.password
```

### Create a GitHub token

Open [GitHub's fine-grained token form](https://github.com/settings/personal-access-tokens/new).
Choose a name and expiration, then:

1. Set **Resource owner** to the account or organization owning the repositories.
   A token scoped to your personal account cannot read another organization's
   private repositories just because your normal login can.
2. Select the repositories under **Only select repositories**.
3. Give **Contents**, **Issues**, **Pull requests**, and **Actions** read-only
   access. **Metadata** is normally included automatically.
4. Generate the token. If the organization requires approval, wait for approval.

See [GitHub's token documentation](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)
for organization restrictions and fine-grained token limitations. A fine-grained
token has one resource owner; a personal fork and an organization's private
repository may therefore need a different token strategy.

Save the token in `github` without displaying it or placing it in shell history
(these commands use Bash):

```sh
(
  umask 077
  read -r -s -p 'Paste GitHub token: ' token
  printf '\n'
  printf '%s\n' "$token" > ~/.config/split-gh/read-token
)
chmod 600 ~/.config/split-gh/read-token
```

An existing `gh` token also works. To use your stored login instead:

```sh
(
  unset GH_TOKEN GITHUB_TOKEN
  umask 077
  gh auth token --hostname github.com > ~/.config/split-gh/read-token
)
chmod 600 ~/.config/split-gh/read-token
```

That token may have write permissions. split-gh still enforces read-only requests,
but a dedicated read-only token limits the consequences of a server compromise.
The service uses the token file, not your normal `gh` login.

### Test the exact token before continuing

In `github`, using the real `gh`:

```sh
(
  export GH_TOKEN="$(cat ~/.config/split-gh/read-token)"
  gh api repos/example/project --jq .full_name
  gh pr list -R example/project
)
```

Expect the repository name and either a PR list or `no open pull requests`.
A 404 or `Could not resolve to a Repository` can mean the token lacks access,
even when ordinary `gh` works. Check the owner, selected repositories, approval,
and spelling. Keep the token in `github`; only the client password goes to `agent`.

## 3. Allow forwarding in dom0

Create `/etc/qubes/policy.d/30-split-gh.policy`:

```text
qubes.ConnectTCP +8401 agent github allow
qubes.ConnectTCP +8401 @anyvm @anyvm deny
```

In `github`, `/etc/qubes-rpc/qubes.ConnectTCP` should be a symlink to
`/dev/tcp/127.0.0.1`. The connection reaches the local server and starts no service.
Keep `qubes.Filecopy` from `agent` at `ask` so you confirm patch transfers.

## 4. Install the client in `agent`

Replace `source` with the name of the qube that sent the binary:

```sh
mkdir -p ~/.local/bin
install -m 755 ~/QubesIncoming/source/split-gh ~/.local/bin/split-gh
ln -sfn split-gh ~/.local/bin/gh
ln -sfn split-gh ~/.local/bin/git-remote-split-gh
export PATH="$HOME/.local/bin:$PATH"
hash -r

install -d -m 700 ~/.config/split-gh
install -m 600 ~/QubesIncoming/github/agent.password ~/.config/split-gh/password
qvm-connect-tcp 8401:github:8401
```

Add `export PATH="$HOME/.local/bin:$PATH"` to `~/.bashrc` (or your shell's startup
file). Reload it in existing Bash terminals with `source ~/.bashrc` and `hash -r`.
Verify:

```sh
command -v gh
gh
```

The path should be `~/.local/bin/gh`, and `gh` should describe split-gh.
If you see the real GitHub CLI, fix PATH ordering; merely editing `.bashrc`
does not update existing shells. `~/.local/bin/gh` explicitly invokes the shim.

Add this line to `/rw/config/rc.local` so forwarding starts after reboot:

```sh
qvm-connect-tcp 8401:github:8401
```

Configure GitHub URLs to use the remote helper:

```sh
git config --global url.split-gh::https://github.com/.insteadOf https://github.com/
git config --global --add url.split-gh::https://github.com/.insteadOf git@github.com:
git config --global --add url.split-gh::https://github.com/.insteadOf ssh://git@github.com/
git config --global protocol.split-gh.allow always
```

## 5. Start and verify

In a terminal in `github`, leave this running:

```sh
split-gh serve
```

In `agent`:

```sh
gh pr list -R example/project
git ls-remote https://github.com/example/project.git
```

Restart `split-gh serve` after changing its config or token file. Ctrl-C stops
access through the service. Public repositories outside `read_repos` are fetched
directly without credentials, so they also require network access from `agent`.

## 6. Configure the patch receiver

For two qubes, `github` already has `local_repos` from step 2. For three, install
split-gh in `source` and create `~/.config/split-gh/config.json` there:

```json
{
  "qubes": {
    "agent": {
      "local_repos": ["project"]
    }
  }
}
```

The separate receiver needs no read token or running server. It identifies the
sender from `~/QubesIncoming/agent/`, so keep received patches in that directory.
The main checkout must exist at `~/project` in both `agent` and the receiver.
For nested checkouts, use the same home-relative path, such as `projects/project`,
in both qubes and in `local_repos`.

## Everyday use

### Clone or update a checkout

In `agent`, clone if needed:

```sh
git clone https://github.com/example/project.git ~/project
```

To update an existing checkout's `master` branch:

```sh
cd ~/project
git status
git switch master
git pull --ff-only origin master
```

Commit or stash outstanding changes before switching. Substitute `main` if that
is the repository's branch name. `--ff-only` stops if local history has diverged.

### Work with a fork or another branch

A common remote arrangement is `origin` for your fork and `upstream` for the
original repository. Check yours rather than assuming:

```sh
git remote -v
git ls-remote --heads origin refs/heads/feature-branch
```

If the branch exists on your fork:

```sh
git fetch origin feature-branch:feature-branch
```

`couldn't find remote ref feature-branch` means that remote did not advertise
that branch. Check spelling and the remote URL. A branch that exists only in
your trusted checkout must first be pushed from that qube:

```sh
git push origin feature-branch
```

Then fetch again in `agent`. For a private fork, allowlist the fork's
`contributor/project` name as well as the upstream repository, and ensure the
token can read it. Alternatively, `*/project` covers both owners. An exact
upstream allowlist entry does not cover forks.

For a PR branch, you can instead fetch by PR number from the repository holding
the PR:

```sh
git fetch upstream pull/123/head:feature-branch
```

### Send commits for review

Work in a linked worktree, not the main checkout. For a new branch:

```sh
cd ~/project
git worktree add -b feature-branch feature-branch origin/master
cd feature-branch
# Edit, test, and commit your changes here.
split-gh send
```

For an already fetched local branch, use `git worktree add feature-branch
feature-branch` instead of creating it with `-b`.

Choose your patch receiver (`github` or `source`) in the transfer dialog. In
that qube, run the exact commands printed by send, for example:

```sh
split-gh diff ~/QubesIncoming/agent/project-feature-branch-abcdef0.patch
split-gh apply ~/QubesIncoming/agent/project-feature-branch-abcdef0.patch
```

Review before applying. Apply reproduces the worktree and prints a push command
to run from the main checkout. If the base commit is missing, fetch in the
receiver and retry. Run builds and tests in `agent`; files in the received
worktree are agent-controlled. See [patch handling and signing](docs/install.md#sending-commits)
for signing, history restrictions, and rollback behavior.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Regular `gh` works, split-gh reports 404 or a GraphQL repository error | Test the token file directly in `github`; it can differ from your normal login. |
| Token works directly, but the shim refuses the repository | Check `read_repos`, including the fork owner, and restart serve. |
| `gh` does not describe split-gh | Check `command -v gh`, reload shell startup config, and run `hash -r`. |
| Nothing listens on local port 8401 | Start `qvm-connect-tcp 8401:github:8401` in `agent`. |
| Forwarding exists but the service does not answer | Check the dom0 policy, the destination qube, and that serve is running. |
| Request authentication fails | Check that the client password matches `github`'s configured password file. |
| `couldn't find remote ref` | Check the remote and branch name; push a local-only branch from the trusted qube first. |
| Apply refuses the repository | Check the sender's `local_repos` and matching home-relative checkout paths. |

## Further reference

- [Detailed installation, security behavior, limits, and acceptance tests](docs/install.md)
- [Instructions to give your agents](docs/agents.md)
- [Example dom0 policy](qubes/30-split-gh.policy)

`nix flake check` runs the tests in Nix's build sandbox. `nix build` builds the
binary without running repository tests.
