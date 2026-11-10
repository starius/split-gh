# Working with GitHub and gh-auth

This qube has no GitHub credentials and can't push, and that is on purpose.
The user reviews, signs and pushes every change from gh-auth.

- **Read GitHub with `gh`.** Here `gh` is split-gh, which runs the real gh in
  gh-auth for reads only. Pull requests, issues, checks, runs, releases,
  searches with `--repo` or `--owner`, and `gh api` GET requests work as usual.
  For review comments with their resolved state, use `gh pr threads N`. `gh`
  with no arguments explains the rest. Commands that write, such as
  `gh pr create`, `gh pr comment` or `gh api -X POST`, are refused. Don't try
  to work around that: write a draft for the user instead.
- **View projects with `gh project view N --owner OWNER`.** The user must allow
  the project in `read_projects`, separately from repositories. `gh project
  item-list N --owner OWNER` and `field-list` read its items and fields.
  `gh project list --owner OWNER` (alias `ls`) needs an `OWNER/*` project grant.
  All support `--format json`, `--template`, and local `--jq`; list commands also
  support `--limit`/`-L`. `item-list` supports `--query`, repeated `--field`, and
  repeated `--field-id` with a recent real gh. Specify the actual owner and
  project number where applicable; `@me`, interactive selection, `--web`, and
  project writes are refused.
- **Clone and fetch with git as usual.** `git clone` and `git fetch` from
  GitHub go through gh-auth for the repositories the user allows, private
  ones included, and straight to GitHub for other public repositories.
  Pushing doesn't work. Use `git clone https://github.com/OWNER/REPO`
  instead of `gh repo clone`, and `git fetch origin pull/N/head` instead of
  `gh pr checkout`.
- **Hand over commits with `split-gh send`.** Commit your work in a worktree,
  not the main checkout, and run `split-gh send` there. The user confirms the
  file transfer, then reviews and applies it in gh-auth. Show the user the two
  commands it prints. Send again after more commits or after amending: the
  user sees what changed since the last version.
- **Build on the user's changes.** Before you continue on a branch you sent,
  `git fetch origin`: if the user pushed the branch, they may have changed it,
  and `git log HEAD..origin/<branch>` shows how. Rebase on it.
- Keep history linear, with no merges, and don't use a line `---` in commit
  messages; send says when a commit can't be sent as is.
