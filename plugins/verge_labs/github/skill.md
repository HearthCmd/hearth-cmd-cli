---
name: github
description: >
  Use when working with GitHub issues, pull requests, files, or commits
  via a hearth resource connection. Covers read and write operations
  against the GitHub REST API through the github plugin.
---

# GitHub plugin

Invoke via `hearth resource invoke <connection> <verb> '<args-json>'`.

`<connection>` is the name shown in your resource list — either the slug
(e.g. `github-work`) or the connection UUID. Both resolve. Examples below
use `github-work`; substitute the connection you were actually granted.

## Passing arguments

Args are **one JSON object**, quoted as a single shell argument. There is no
`--arg` flag:

```
hearth resource invoke github-work get_repo
hearth resource invoke github-work get_issue '{"number":42}'
```

`number` and `per_page` are declared as integers — pass them as bare JSON
numbers, not strings.

The `patch` arg of `update_issue` is a **string containing JSON**, spliced
into the PATCH body verbatim. It must be escaped inside the outer object.
This looks awkward and is correct:

```
hearth resource invoke github-work update_issue '{"number":42,"patch":"{\"title\":\"New title\"}"}'
```

Passing `patch` as a nested JSON object instead of an escaped string is an
error.

## Configured defaults

`owner` and `repo` are set in the connection's config — you don't need
to pass them for operations on the primary repository. All verbs default
to those values.

## Lookup before you write

Before creating an issue or PR, search for duplicates:

```
hearth resource invoke github-work search_issues '{"query":"is:issue is:open <keywords>"}'
```

Before updating an issue, fetch its current state:

```
hearth resource invoke github-work get_issue '{"number":42}'
```

This avoids clobbering changes made since you last read the record.

## Reading files

`get_file` returns base64-encoded content. Decode it before reading:

```
hearth resource invoke github-work get_file '{"path":"src/foo.go"}' | jq -r '.content' | base64 -d
```

Pass `ref` to read from a specific branch or commit:

```
hearth resource invoke github-work get_file '{"path":"README.md","ref":"main"}'
```

## Issues

- Use `create_issue` only when the task doesn't already exist. One search first.
- Cross-reference with `Fixes #<number>` or `Closes #<number>` in issue or PR bodies — GitHub auto-closes the referenced issue when the PR merges.

### Closing and reopening

Use the dedicated verbs for state-only changes:

```
hearth resource invoke github-work close_issue '{"number":42}'
hearth resource invoke github-work open_issue '{"number":42}'
```

### Updating issue fields

`update_issue` takes a `patch` arg — a JSON object containing only the fields
you want to change, carried as an escaped **string** (see "Passing arguments"
above). It is passed directly as the PATCH body.

```
# Change the title
hearth resource invoke github-work update_issue '{"number":42,"patch":"{\"title\":\"New title\"}"}'

# Replace the labels array (include all labels you want to keep)
hearth resource invoke github-work update_issue '{"number":42,"patch":"{\"labels\":[\"bug\",\"help wanted\"]}"}'

# Edit the body
hearth resource invoke github-work update_issue '{"number":42,"patch":"{\"body\":\"Updated description\"}"}'

# Multiple fields at once
hearth resource invoke github-work update_issue '{"number":42,"patch":"{\"title\":\"Done\",\"labels\":[\"done\"]}"}'
```

Prefer `close_issue` / `open_issue` for state-only changes — `update_issue`
is for field edits.

## Pull requests

- `head` is the branch carrying your changes; `base` is the branch you're merging into (usually `main` or `master`).
- `create_pull_request` requires the head branch to already exist on the remote.

```
hearth resource invoke github-work create_pull_request '{
  "title": "Fix the widget",
  "head": "fix/widget",
  "base": "main",
  "body": "Fixes #42."
}'
```

## Code review

`add_pr_review_comment` submits a review, not just a comment. Three event types:

- `COMMENT` — general feedback, no approval signal
- `APPROVE` — signals the PR is ready to merge
- `REQUEST_CHANGES` — blocks merge until addressed

```
hearth resource invoke github-work add_pr_review_comment '{
  "number": 17,
  "event": "APPROVE",
  "body": "LGTM"
}'
```

Prefer one review call over multiple `add_issue_comment` calls on the PR — it
gives the author a coherent review to respond to.

## List verbs return page 1 only

`list_issues`, `list_pull_requests`, and `list_commits` return at most
`per_page` results (default 30, max 100). There is no pagination in v1.
If you need more results, use `search_issues` with a scoped query, or
request a larger `per_page`:

```
hearth resource invoke github-work list_issues '{"state":"open","per_page":100}'
```

## Rate limits

GitHub's REST API allows 5 000 requests/hour per token. Don't loop
tightly over list verbs — fetch once, work with the result.
