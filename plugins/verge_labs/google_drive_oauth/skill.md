---
name: google_drive_oauth
description: >
  Use when reading or writing files in a Google Drive account via a
  hearth resource connection. Covers listing, searching, downloading,
  uploading, and organising files through the google_drive_oauth plugin.
---

# Google Drive plugin

Invoke via `hearth resource invoke <connection> <verb> '<args-json>'`.

`<connection>` is the name shown in your resource list — either the slug
(e.g. `my_drive`) or the connection UUID. Both resolve. Examples below
use `my_drive`; substitute the connection you were actually granted.

## Passing arguments

Args are **one JSON object**, quoted as a single shell argument. There is no
`--arg` flag. Omit the object entirely for a verb that takes no args:

```
hearth resource invoke my_drive list_files
hearth resource invoke my_drive get_file_metadata '{"file_id":"<id>"}'
```

Args whose name ends in `_json` take a **string containing JSON**, so that
JSON is escaped inside the outer object (`"items_json": "[{\"id\":\"a\"}]"`,
not a nested array). No Drive verb currently takes a `_json` arg — every arg
here is a plain string — but the rule holds if one is added.

## Finding files

Start with a search rather than a listing when you know what you're looking for:

```
hearth resource invoke my_drive search_files '{"query":"name contains '\''budget'\''"}'
hearth resource invoke my_drive search_files '{"query":"fullText contains '\''Q3 revenue'\''"}'
hearth resource invoke my_drive search_files '{"query":"mimeType = '\''text/plain'\''"}'
```

Drive query syntax uses single quotes, which collide with the single quotes
wrapping the JSON. The `'\''` sequence above closes, escapes, and reopens the
shell quote. Double-quoting the whole argument and escaping the inner double
quotes works too:

```
hearth resource invoke my_drive search_files "{\"query\":\"name contains 'budget'\"}"
```

Queries follow Google's Drive query syntax:
https://developers.google.com/drive/api/guides/search-files

To list the contents of a known folder, use the folder's Drive ID (from its
URL or from a previous search result):

```
hearth resource invoke my_drive list_folder_contents '{"folder_id":"1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgVE2upms"}'
```

Pass `"folder_id": "root"` for the top of My Drive.

## Reading file content

For plain text, Markdown, code, CSV, PDF — use `download_file`:

```
hearth resource invoke my_drive download_file '{"file_id":"<id>"}'
```

For Google Workspace files (Docs, Sheets, Slides) — use `export_file` with
a target MIME type:

```
# Google Doc → plain text
hearth resource invoke my_drive export_file '{"file_id":"<id>","mime_type":"text/plain"}'

# Google Doc → Word
hearth resource invoke my_drive export_file '{"file_id":"<id>","mime_type":"application/vnd.openxmlformats-officedocument.wordprocessingml.document"}'

# Google Sheet → CSV
hearth resource invoke my_drive export_file '{"file_id":"<id>","mime_type":"text/csv"}'

# Any file → PDF
hearth resource invoke my_drive export_file '{"file_id":"<id>","mime_type":"application/pdf"}'
```

`download_file` on a Google Workspace file returns an error — use `export_file`
for those types.

## Getting a file's metadata

Before moving, renaming, or trashing a file, read its current state:

```
hearth resource invoke my_drive get_file_metadata '{"file_id":"<id>"}'
```

The response includes `parents` (the current parent folder IDs) — you need
this for `move_file`.

## Creating files

`create_file` creates a metadata-only record and returns the new file's `id`.
You must supply a `parent_id`; pass `"root"` for My Drive root.

```
# Create an empty text file
hearth resource invoke my_drive create_file '{
  "name": "notes.md",
  "mime_type": "text/markdown",
  "parent_id": "root"
}'

# Create an empty Google Doc
hearth resource invoke my_drive create_file '{
  "name": "Draft",
  "mime_type": "application/vnd.google-apps.document",
  "parent_id": "<folder_id>"
}'
```

To set the file's content immediately, follow with `upload_file_content`:

```
hearth resource invoke my_drive upload_file_content '{
  "file_id": "<id>",
  "mime_type": "text/markdown",
  "content": "# My document\n\nContent here."
}'
```

`upload_file_content` replaces the entire file content. It works for plain
text and other non-binary formats. For Google Docs/Sheets, write to a plain
text file and let the user convert, or use the Docs API (not this plugin).

## Organising files

Rename:
```
hearth resource invoke my_drive rename_file '{"file_id":"<id>","name":"New name.md"}'
```

Move (requires the current parent ID — get it from `get_file_metadata`):
```
hearth resource invoke my_drive move_file '{
  "file_id": "<id>",
  "new_parent_id": "<destination_folder_id>",
  "old_parent_id": "<current_parent_id>"
}'
```

Create a folder:
```
hearth resource invoke my_drive create_folder '{"name":"2026 Reports","parent_id":"root"}'
```

Trash (recoverable from Drive's Trash):
```
hearth resource invoke my_drive trash_file '{"file_id":"<id>"}'
```

Trashing is always preferred over permanent deletion. There is no
permanent-delete verb — use the Drive web UI for that.

## Sharing files

```
# Share with a specific user
hearth resource invoke my_drive share_file '{
  "file_id": "<id>",
  "role": "writer",
  "type": "user",
  "email_address": "colleague@example.com"
}'

# Share with everyone in a domain
hearth resource invoke my_drive share_file '{
  "file_id": "<id>",
  "role": "reader",
  "type": "domain",
  "email_address": "vergelabs.org"
}'

# Make publicly readable
hearth resource invoke my_drive share_file '{
  "file_id": "<id>",
  "role": "reader",
  "type": "anyone",
  "email_address": ""
}'
```

Roles: `reader`, `commenter`, `writer`, `fileOrganizer`, `organizer`, `owner`.

`email_address` is required on every `share_file` call — pass an empty string
when `type` is `anyone`.

## File IDs vs names

Drive uses opaque file IDs, not paths. Whenever you need to act on a file
you haven't seen yet:
1. `search_files` or `list_folder_contents` to find it
2. Copy the `id` from the result
3. Pass that `id` to the target verb

Never guess or construct a file ID.

## list_files returns 50 results

`list_files` returns the 50 most recently modified non-trashed files. There
is no pagination in v1. For targeted results, use `search_files` or
`list_folder_contents` instead.
