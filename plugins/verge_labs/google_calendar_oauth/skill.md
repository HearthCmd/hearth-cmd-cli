---
name: google_calendar_oauth
description: >
  Use when scheduling meetings, checking availability, or managing calendar
  events in a Google Workspace account via a hearth resource connection.
  Covers listing events, checking free/busy, creating events with invites,
  updating, and cancelling through the google_calendar_oauth plugin.
---

# Google Calendar plugin

Invoke via `hearth resource invoke <connection> <verb> '<args-json>'`.

`<connection>` is the name shown in your resource list — either the slug
(e.g. `my_calendar`) or the connection UUID. Both resolve. Examples below
use `my_calendar`; substitute the connection you were actually granted.

## Passing arguments

Args are **one JSON object**, quoted as a single shell argument. There is no
`--arg` flag:

```
hearth resource invoke my_calendar list_calendars
hearth resource invoke my_calendar list_events '{"calendar_id":"primary"}'
```

Args whose name ends in `_json` take a **string containing JSON**, so that
JSON is escaped inside the outer object. This looks awkward and is correct:

```
hearth resource invoke my_calendar check_availability '{"time_min":"2026-07-20T09:00:00-07:00","time_max":"2026-07-20T18:00:00-07:00","items_json":"[{\"id\":\"alice@example.com\"}]"}'
```

## Before scheduling: always check availability

Never propose a time without checking first. `check_availability` returns busy
blocks for everyone you want to invite:

```
hearth resource invoke my_calendar check_availability '{
  "time_min": "2026-07-20T00:00:00-07:00",
  "time_max": "2026-07-20T23:59:59-07:00",
  "items_json": "[{\"id\":\"alice@vergelabs.org\"},{\"id\":\"bob@vergelabs.org\"}]"
}'
```

The response has a `calendars` object keyed by email. Each entry has a `busy`
array of `{start, end}` blocks. Find a gap where nobody is busy.

## Creating an event

Once you have a free slot, create the event. Invites go out automatically
(`sendUpdates=all` is built into the verb):

```
hearth resource invoke my_calendar create_event '{
  "calendar_id": "primary",
  "summary": "Q3 planning sync",
  "start_datetime": "2026-07-20T14:00:00-07:00",
  "end_datetime": "2026-07-20T15:00:00-07:00",
  "timezone": "America/Los_Angeles",
  "attendees_json": "[{\"email\":\"alice@vergelabs.org\"},{\"email\":\"bob@vergelabs.org\"}]",
  "description": "Agenda: roadmap priorities for Q3.",
  "location": "Conf room B"
}'
```

`attendees_json` is a JSON array of `{"email": "..."}` objects. Everyone in
the list receives an email invite.

## Datetime format

All datetimes must be RFC3339 with an explicit timezone offset:

```
2026-07-20T14:00:00-07:00   ✓  Pacific Daylight Time
2026-07-20T14:00:00Z        ✓  UTC
2026-07-20T14:00:00         ✗  no offset — will be rejected
```

`timezone` must be an IANA name (`America/Los_Angeles`, `America/New_York`,
`Europe/London`, `UTC`). It controls how the event appears in each attendee's
calendar UI, independent of the offset in the datetime string.

## Listing upcoming events

```
hearth resource invoke my_calendar list_events '{
  "calendar_id": "primary",
  "time_min": "2026-07-15T00:00:00-07:00",
  "time_max": "2026-07-22T23:59:59-07:00"
}'
```

Results are sorted by start time. Each item includes `id` (needed for
`get_event`, `update_event`, `cancel_event`), `summary`, `start`, `end`, and
`attendees`.

## Getting full event details

```
hearth resource invoke my_calendar get_event '{"calendar_id":"primary","event_id":"<id from list_events>"}'
```

Returns the full event including `conferenceData` (Meet/Zoom links),
`recurrence` rules, and per-attendee `responseStatus`.

## Updating an event

Always call `get_event` first, then supply all fields with your changes merged
in. The API replaces whatever you send, so omitting a field clears it.

```
# 1. Fetch current state
hearth resource invoke my_calendar get_event '{"calendar_id":"primary","event_id":"<id>"}'

# 2. Update with all fields (changed + unchanged)
hearth resource invoke my_calendar update_event '{
  "calendar_id": "primary",
  "event_id": "<id>",
  "summary": "Q3 planning sync (rescheduled)",
  "description": "Agenda: roadmap priorities for Q3.",
  "location": "Conf room B",
  "start_datetime": "2026-07-21T10:00:00-07:00",
  "end_datetime": "2026-07-21T11:00:00-07:00",
  "timezone": "America/Los_Angeles",
  "attendees_json": "[{\"email\":\"alice@vergelabs.org\"},{\"email\":\"bob@vergelabs.org\"}]"
}'
```

`attendees_json` replaces the entire attendee list — include everyone who
should remain invited, not just the new additions.

## Cancelling an event

```
hearth resource invoke my_calendar cancel_event '{"calendar_id":"primary","event_id":"<id>"}'
```

Sends cancellation emails to all attendees and permanently deletes the event.
Cannot be undone.

## Listing available calendars

```
hearth resource invoke my_calendar list_calendars
```

Returns all calendars the user has access to. Use the `id` field as
`calendar_id` in other verbs. `"primary"` always works for the main calendar
and is the right default in almost all cases.

## calendar_id

Pass `"primary"` unless you have a specific reason to target a different
calendar. Secondary calendars (team calendars, shared resource rooms, etc.)
have their own IDs visible in `list_calendars`.

## items_json vs attendees_json — keep them straight

`check_availability` takes `items_json`, a JSON array of `{"id": "email"}`
objects. The freebusy API uses `id`, **not** `email`:

```json
[{"id": "alice@vergelabs.org"}, {"id": "bob@vergelabs.org"}]
```

`create_event` and `update_event` take `attendees_json`, which uses
`{"email": ...}`:

```json
[{"email": "alice@vergelabs.org"}, {"email": "bob@vergelabs.org"}]
```

Mixing them up returns empty results rather than an error.
