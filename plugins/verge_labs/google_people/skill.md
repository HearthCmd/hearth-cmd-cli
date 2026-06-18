---
name: google_people
description: >
  Use when looking up people in a Google Workspace organization — finding
  email addresses, phone numbers, job titles, or departments for colleagues.
  Covers searching, listing, and fetching profiles through the google_people
  plugin.
---

# Google People plugin (Workspace Directory)

Invoke via `hearth resource <connection-slug> <verb> [--arg key=value ...]`.
The connection slug is shown in your resource list (e.g. `flogg_people`).

## Finding someone

`search_people` is the right starting point for almost every lookup. Pass
any fragment of a name or email:

```
hearth resource flogg_people search_people --arg query="alice"
hearth resource flogg_people search_people --arg query="smith"
hearth resource flogg_people search_people --arg query="alice@vergelabs.org"
hearth resource flogg_people search_people --arg query="engineering"
```

Each result includes:
- `resourceName` — opaque ID (e.g. `people/c1234567890`); pass to `get_person`
- `names[0].displayName` — full display name
- `emailAddresses[0].value` — primary work email
- `phoneNumbers` — work/mobile numbers if set
- `organizations[0].title` / `.department` — job title and department

## Getting full profile details

Once you have a `resourceName` from search, fetch the complete profile:

```
hearth resource flogg_people get_person \
  --arg resource_name="people/c1234567890"
```

Returns everything `search_people` returns plus `biographies` (About field)
and `photos` (profile photo URL).

## Listing the full directory

Only use `list_people` when you need to enumerate everyone — for targeted
lookups `search_people` is faster and returns cleaner results:

```
hearth resource flogg_people list_people
```

Returns up to 100 people. The response includes a `nextPageToken` if
there are more; the plugin has no pagination verb in v1 so if the org
is large, use `search_people` with specific queries instead.

## Response shape

`search_people` returns:
```json
{
  "people": [
    {
      "resourceName": "people/c1234567890",
      "names": [{"displayName": "Alice Smith", "givenName": "Alice", "familyName": "Smith"}],
      "emailAddresses": [{"value": "alice@vergelabs.org", "type": "work"}],
      "phoneNumbers": [{"value": "+1 415 555 0100", "type": "work"}],
      "organizations": [{"title": "Staff Engineer", "department": "Platform"}]
    }
  ]
}
```

Fields are arrays because a person can have multiple emails/phones. The
primary entry is always index 0.

## Finding someone's email for calendar invites

The typical sequence when scheduling a meeting:

```
# 1. Find the person
hearth resource flogg_people search_people --arg query="bob"

# 2. Copy their email from emailAddresses[0].value
# 3. Use it in check_availability + create_event
hearth resource flogg_calendar check_availability \
  --arg time_min="..." \
  --arg time_max="..." \
  --arg items_json='[{"id":"bob@vergelabs.org"}]'
```
