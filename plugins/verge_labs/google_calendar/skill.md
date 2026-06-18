---
name: google_calendar
description: >
  Use when scheduling meetings, checking availability, or managing calendar
  events in a Google Workspace account via a hearth resource connection.
  Covers listing events, checking free/busy, creating events with invites,
  updating, and cancelling through the google_calendar plugin.
---

# Google Calendar plugin

Invoke via `hearth resource <connection-slug> <verb> [--arg key=value ...]`.
The connection slug is shown in your resource list (e.g. `flogg_calendar`).

## Before scheduling: always check availability

Never propose a time without checking first. The `check_availability` verb
returns busy blocks for everyone you want to invite:

```
hearth resource flogg_calendar check_availability \
  --arg time_min="2024-06-15T00:00:00-07:00" \
  --arg time_max="2024-06-15T23:59:59-07:00" \
  --arg items_json='[{"id":"alice@vergelabs.org"},{"id":"bob@vergelabs.org"}]'
```

The response has a `calendars` object keyed by email. Each entry has a
`busy` array of `{start, end}` blocks. Find a gap where nobody is busy.

## Creating an event

Once you have a free slot, create the event with `sendUpdates=all` (built
into the verb — invites go out automatically):

```
hearth resource flogg_calendar create_event \
  --arg calendar_id="primary" \
  --arg summary="Q3 planning sync" \
  --arg start_datetime="2024-06-15T14:00:00-07:00" \
  --arg end_datetime="2024-06-15T15:00:00-07:00" \
  --arg timezone="America/Los_Angeles" \
  --arg attendees_json='[{"email":"alice@vergelabs.org"},{"email":"bob@vergelabs.org"}]' \
  --arg description="Agenda: roadmap priorities for Q3." \
  --arg location="Conf room B"
```

`attendees_json` must be a JSON array of `{"email": "..."}` objects.
Everyone in the list receives an email invite.

## Datetime format

All datetimes must be RFC3339 with an explicit timezone offset:

```
2024-06-15T14:00:00-07:00   ✓  Pacific Daylight Time
2024-06-15T14:00:00Z        ✓  UTC
2024-06-15T14:00:00         ✗  no offset — will be rejected
```

`timezone` must be an IANA name (`America/Los_Angeles`, `America/New_York`,
`Europe/London`, `UTC`). It controls how the event appears in each
attendee's calendar UI, independent of the offset in the datetime string.

## Listing upcoming events

```
hearth resource flogg_calendar list_events \
  --arg calendar_id="primary" \
  --arg time_min="2024-06-10T00:00:00-07:00" \
  --arg time_max="2024-06-17T23:59:59-07:00"
```

Results are sorted by start time. Each item includes `id` (needed for
`get_event`, `update_event`, `cancel_event`), `summary`, `start`, `end`,
and `attendees`.

## Getting full event details

```
hearth resource flogg_calendar get_event \
  --arg calendar_id="primary" \
  --arg event_id="<id from list_events>"
```

Returns the full event including `conferenceData` (Meet/Zoom links),
`recurrence` rules, and per-attendee `responseStatus`.

## Updating an event

Always call `get_event` first, then supply all fields with your changes
merged in. The API replaces whatever you send, so sending an empty value
clears that field.

```
# 1. Fetch current state
hearth resource flogg_calendar get_event \
  --arg calendar_id="primary" \
  --arg event_id="<id>"

# 2. Update with all fields (changed + unchanged)
hearth resource flogg_calendar update_event \
  --arg calendar_id="primary" \
  --arg event_id="<id>" \
  --arg summary="Q3 planning sync (rescheduled)" \
  --arg description="Agenda: roadmap priorities for Q3." \
  --arg location="Conf room B" \
  --arg start_datetime="2024-06-16T10:00:00-07:00" \
  --arg end_datetime="2024-06-16T11:00:00-07:00" \
  --arg timezone="America/Los_Angeles" \
  --arg attendees_json='[{"email":"alice@vergelabs.org"},{"email":"bob@vergelabs.org"}]'
```

`attendees_json` replaces the entire attendee list — include everyone who
should remain invited, not just the new additions.

## Cancelling an event

```
hearth resource flogg_calendar cancel_event \
  --arg calendar_id="primary" \
  --arg event_id="<id>"
```

Sends cancellation emails to all attendees and permanently deletes the
event. Cannot be undone.

## Listing available calendars

```
hearth resource flogg_calendar list_calendars
```

Returns all calendars the user has access to. Use the `id` field as
`calendar_id` in other verbs. `"primary"` always works for the main
calendar and is the right default in almost all cases.

## calendar_id

Pass `"primary"` unless you have a specific reason to target a different
calendar. Secondary calendars (team calendars, shared resource rooms, etc.)
have their own IDs visible in `list_calendars`.

## check_availability items_json format

The `items_json` arg for `check_availability` must be a JSON array of
`{"id": "email"}` objects (not `{"email": ...}` — the freebusy API uses
`id` not `email`):

```json
[{"id": "alice@vergelabs.org"}, {"id": "bob@vergelabs.org"}]
```

This is different from `attendees_json` in `create_event` which uses
`{"email": ...}`. Keep them straight or the API will return empty results.
