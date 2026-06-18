---
name: github
description: >
  Use when working with GitHub issues, pull requests, files, or commits
  via a hearth resource connection. Covers read and write operations
  against the GitHub REST API through the github plugin.
---

# GitHub plugin

Invoke via `hearth resource <connection-id> <verb> [--arg key=value ...]`.
The connection id is shown in your resource list (e.g. `github-work`).

## Configured defaults

`owner` and `repo` are set in the connection's config — you don't need
to pass them for operations on the primary repository. All verbs default
to those values.

## Lookup before you write

Before creating an issue or PR, search for duplicates:

```
hearth resource github-work search_issues --arg query="is:issue is:open <keywords>"
```

Before updating an issue, fetch its current state:

```
hearth resource github-work get_issue --arg number=42
```

This avoids clobbering changes made since you last read the record.

## Reading files

`get_file` returns base64-encoded content. Decode it before reading:

```
hearth resource github-work get_file --arg path=src/foo.go | jq -r '.content' | base64 -d
```

Pass `ref` to read from a specific branch or commit:

```
hearth resource github-work get_file --arg path=README.md --arg ref=main
```

## Issues

- Use `create_issue` only when the task doesn't already exist. One search first.
- Cross-reference with `Fixes #<number>` or `Closes #<number>` in issue or PR bodies — GitHub auto-closes the referenced issue when the PR merges.

### Closing and reopening

Use the dedicated verbs for state-only changes:

```
hearth resource github-work close_issue --arg number=42
hearth resource github-work open_issue --arg number=42
```

### Updating issue fields

`update_issue` takes a `patch` arg — a raw JSON object containing only the
fields you want to change. This is passed directly as the PATCH body.

```
# Change the title
hearth resource github-work update_issue --arg number=42 --arg patch='{"title":"New title"}'

# Replace the labels array (include all labels you want to keep)
hearth resource github-work update_issue --arg number=42 --arg patch='{"labels":["bug","help wanted"]}'

# Edit the body
hearth resource github-work update_issue --arg number=42 --arg patch='{"body":"Updated description"}'

# Multiple fields at once
hearth resource github-work update_issue --arg number=42 --arg patch='{"title":"Done","labels":["done"]}'
```

Prefer `close_issue` / `open_issue` for state-only changes — `update_issue`
is for field edits.

## Pull requests

- `head` is the branch carrying your changes; `base` is the branch you're merging into (usually `main` or `master`).
- `create_pull_request` requires the head branch to already exist on the remote.

## Code review

`add_pr_review_comment` submits a review, not just a comment. Three event types:

- `COMMENT` — general feedback, no approval signal
- `APPROVE` — signals the PR is ready to merge
- `REQUEST_CHANGES` — blocks merge until addressed

```
hearth resource github-work add_pr_review_comment \
  --arg number=17 \
  --arg event=APPROVE \
  --arg body="LGTM"
```

Prefer one review call over multiple `add_issue_comment` calls on the PR — it
gives the author a coherent review to respond to.

## List verbs return page 1 only

`list_issues`, `list_pull_requests`, and `list_commits` return at most
`per_page` results (default 30, max 100). There is no pagination in v1.
If you need more results, use `search_issues` with a scoped query, or
request a larger `per_page`.

## Rate limits

GitHub's REST API allows 5 000 requests/hour per token. Don't loop
tightly over list verbs — fetch once, work with the result.
