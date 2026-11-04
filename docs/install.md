# Install the phase 1 read path

The examples use `ai` for the agent qube and `gh-auth` for the authenticated
qube. Replace both names and `example/private` with your own. Commands labeled
for dom0, gh-auth, and ai must run in those respective qubes.

## Authenticated qube

Get the reviewed source into gh-auth using your usual trusted workflow. **Do not
copy a binary from ai.** Build and install it in gh-auth:

```sh
CGO_ENABLED=0 go build -trimpath -o split-gh ./cmd/split-gh
sudo install -m 755 split-gh /usr/local/bin/split-gh
sudo install -d -m 755 /usr/local/etc/qubes-rpc
sudo install -m 755 qubes/splitgh.Read /usr/local/etc/qubes-rpc/splitgh.Read
install -d -m 700 ~/.config/split-gh
```

Create `~/.config/split-gh/config.json` as the user that receives qrexec calls:

```json
{
  "qubes": {
    "ai": {"read_repos": ["example/*"]}
  },
  "read_token_file": "~/.config/split-gh/read-token"
}
```

Put the GitHub token in `~/.config/split-gh/read-token` and run `chmod 600` on
it. Prefer a fine-grained read-only token restricted to the required repos,
with Contents, Metadata, Issues, Pull requests and Actions read permissions.
The real `gh` must be installed at `/usr/local/bin/gh` or `/usr/bin/gh`;
the qrexec stub fixes PATH to those system directories. The
service runs it with a temporary home and config, the configured token, and
prompting disabled; it does not use that user's `gh` aliases or extensions.

## Dom0

Create `/etc/qubes/policy.d/30-split-gh.policy` with the following contents,
substituting the actual qube names. `qubes/30-split-gh.policy` contains the
same example policy.

```text
splitgh.Read * ai gh-auth allow
splitgh.Read * @anyvm @anyvm deny
```

## Agent qube

Build the client from source in ai and install a symlink named `gh` earlier on
PATH than any real GitHub CLI. For a user-local installation:

```sh
mkdir -p ~/.local/bin
CGO_ENABLED=0 go build -trimpath -o ~/.local/bin/split-gh ./cmd/split-gh
ln -sfn split-gh ~/.local/bin/gh
export PATH="$HOME/.local/bin:$PATH"
```

Add the PATH setting to the shell's startup file. Set `SPLIT_GH_QUBE` to the
authenticated qube name if it is not `gh-auth`. Confirm `command -v gh` points
to the symlink. The shim first honors a `remote.<name>.gh-resolved` setting, then tries
`upstream` and `origin` in the current checkout. `GH_REPO` overrides checkout inference;
use `-R owner/repo` to select an allowed repository explicitly.

## Verify

From a checkout in ai, using an allowed private repository, try:

```sh
gh pr view 123 --comments
gh api repos/example/private/pulls/123/comments
gh run view --log-failed
gh pr threads 123
```

Use an actual pull request number and repository. `gh pr threads` emits one
JSON object per thread, including paginated comments. Because the service
cannot prompt, `gh run view` without a run or job ID selects the newest run on
the checkout branch (or the newest repository run if there is no branch).
Supply an ID to select a different run. The service writes audit records to
`~/.local/state/split-gh/log.jsonl` in gh-auth. The service records a start
event before executing a read and refuses the request if it cannot write that
event; it also attempts to record the completion status. Each start and finish
pair shares a request ID, including when reads run concurrently.

Searches require an allowed `--repo` or a fully allowed `--owner`. Search text
and filters cannot contain additional repository scope qualifiers or boolean
operators and grouping that could broaden that scope.

REST API paths under `repos/OWNER/REPO` are checked against the caller’s
repository grants even outside a checkout. Ordinary query strings are accepted,
while method-override parameters are refused. The service also accepts the exact
`meta`, `zen`, `emojis`, and `rate_limit` API paths.

The service independently refuses writes from a modified client. The local
integration tests exercise `gh api -X POST ...`, `gh api graphql ...` and
`gh pr comment ...` as
hand-crafted requests directly to the service.

Agent-written `gh api graphql` requests are refused. `gh pr threads` uses a
fixed server-owned GraphQL query and accepts narrower repository grants.

The service refuses `--jq` because gh’s jq implementation can read environment
variables, including the token. The shim handles `--jq` locally in ai using
the `jq` executable; install `jq` there if that option is needed.

