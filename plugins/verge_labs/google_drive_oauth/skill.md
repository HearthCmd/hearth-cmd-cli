---
name: google_drive_oauth
description: >
  Use when reading or writing files in a Google Drive account via a
  hearth resource connection. Covers listing, searching, downloading,
  uploading, and organising files through the google_drive_oauth plugin.
---

# Google Drive plugin

Invoke via `hearth resource <connection-slug> <verb> [--arg key=value ...]`.
The connection slug is shown in your resource list (e.g. `my_drive`).

## Finding files

Start with a search rather than a listing when you know what you're looking for:

```
hearth resource my_drive search_files --arg query="name contains 'budget'"
hearth resource my_drive search_files --arg query="fullText contains 'Q3 revenue'"
hearth resource my_drive search_files --arg query="mimeType = 'text/plain'"
```

Queries follow Google's Drive query syntax:
https://developers.google.com/drive/api/guides/search-files

To list the contents of a known folder, use the folder's Drive ID (from its
URL or from a previous search result):

```
hearth resource my_drive list_folder_contents --arg folder_id=1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgVE2upms
```

Pass `folder_id=root` for the top of My Drive.

## Reading file content

For plain text, Markdown, code, CSV, PDF — use `download_file`:

```
hearth resource my_drive download_file --arg file_id=<id>
```

For Google Workspace files (Docs, Sheets, Slides) — use `export_file` with
a target MIME type:

```
# Google Doc → plain text
hearth resource my_drive export_file --arg file_id=<id> --arg mime_type=text/plain

# Google Doc → Word
hearth resource my_drive export_file --arg file_id=<id> --arg mime_type=application/vnd.openxmlformats-officedocument.wordprocessingml.document

# Google Sheet → CSV
hearth resource my_drive export_file --arg file_id=<id> --arg mime_type=text/csv

# Any file → PDF
hearth resource my_drive export_file --arg file_id=<id> --arg mime_type=application/pdf
```

`download_file` on a Google Workspace file returns an error — use `export_file`
for those types.

## Getting a file's metadata

Before moving, renaming, or trashing a file, read its current state:

```
hearth resource my_drive get_file_metadata --arg file_id=<id>
```

The response includes `parents` (the current parent folder IDs) — you need
this for `move_file`.

## Creating files

`create_file` creates a metadata-only record and returns the new file's `id`.
You must supply a `parent_id`; pass `root` for My Drive root.

```
# Create an empty text file
hearth resource my_drive create_file \
  --arg name="notes.md" \
  --arg mime_type=text/markdown \
  --arg parent_id=root

# Create an empty Google Doc
hearth resource my_drive create_file \
  --arg name="Draft" \
  --arg mime_type=application/vnd.google-apps.document \
  --arg parent_id=<folder_id>
```

To set the file's content immediately, follow with `upload_file_content`:

```
hearth resource my_drive upload_file_content \
  --arg file_id=<id> \
  --arg mime_type=text/markdown \
  --arg content="# My document\n\nContent here."
```

`upload_file_content` replaces the entire file content. It works for plain
text and other non-binary formats. For Google Docs/Sheets, write to a plain
text file and let the user convert, or use the Docs API (not this plugin).

## Organising files

Rename:
```
hearth resource my_drive rename_file --arg file_id=<id> --arg name="New name.md"
```

Move (requires the current parent ID — get it from `get_file_metadata`):
```
hearth resource my_drive move_file \
  --arg file_id=<id> \
  --arg new_parent_id=<destination_folder_id> \
  --arg old_parent_id=<current_parent_id>
```

Create a folder:
```
hearth resource my_drive create_folder \
  --arg name="2024 Reports" \
  --arg parent_id=root
```

Trash (recoverable from Drive's Trash):
```
hearth resource my_drive trash_file --arg file_id=<id>
```

Trashing is always preferred over permanent deletion. There is no
permanent-delete verb — use the Drive web UI for that.

## Sharing files

```
# Share with a specific user
hearth resource my_drive share_file \
  --arg file_id=<id> \
  --arg role=writer \
  --arg type=user \
  --arg email_address=colleague@example.com

# Share with everyone in a domain
hearth resource my_drive share_file \
  --arg file_id=<id> \
  --arg role=reader \
  --arg type=domain \
  --arg email_address=vergelabs.org

# Make publicly readable
hearth resource my_drive share_file \
  --arg file_id=<id> \
  --arg role=reader \
  --arg type=anyone \
  --arg email_address=""
```

Roles: `reader`, `commenter`, `writer`, `fileOrganizer`, `organizer`, `owner`.

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
