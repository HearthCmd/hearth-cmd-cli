---
name: google_people_oauth
description: >
  Use when looking up someone in the user's Google Contacts — finding an email
  address or phone number for a family member, neighbour, contractor, or
  school. Covers searching, listing, and fetching contacts through the
  google_people_oauth plugin.
---

# Google Contacts plugin

Invoke via `hearth resource invoke <connection> <verb> '<args-json>'`.

`<connection>` is the name shown in your resource list — either the slug
(e.g. `my_contacts`) or the connection UUID. Both resolve. Examples below
use `my_contacts`; substitute the connection you were actually granted.

This plugin reads the user's **own contacts**. It does not read a Google
Workspace organization directory — for that, use the `google_people`
(service account) plugin instead.

## Passing arguments

Args are **one JSON object**, quoted as a single shell argument. There is no
`--arg` flag:

```
hearth resource invoke my_contacts list_contacts
hearth resource invoke my_contacts search_contacts '{"query":"sarah"}'
```

Args whose name ends in `_json` take a **string containing JSON**, so that
JSON is escaped inside the outer object. None of this plugin's verbs take one,
but other plugins you chain with do — see the calendar example at the bottom.

## Finding someone

`search_contacts` is the right starting point for almost every lookup. Pass
any fragment of a name, email, or phone number:

```
hearth resource invoke my_contacts search_contacts '{"query":"sarah"}'
hearth resource invoke my_contacts search_contacts '{"query":"plumber"}'
hearth resource invoke my_contacts search_contacts '{"query":"@lincoln-elementary.org"}'
```

Each match includes:
- `resourceName` — opaque ID (e.g. `people/c1234567890`); pass to `get_contact`
- `names[0].displayName` — full display name
- `emailAddresses[0].value` — primary email
- `phoneNumbers` — any numbers on the contact

## Getting full details

Once you have a `resourceName` from search, fetch the whole contact:

```
hearth resource invoke my_contacts get_contact '{"resource_name":"people/c1234567890"}'
```

Returns everything search returns plus `addresses` (useful when a task needs
a physical address — a contractor visit, a delivery) and `biographies` (the
Notes field, which is often where a user has jotted something like "roofer,
quoted $4k, use side gate").

## Listing everyone

Only use `list_contacts` when you genuinely need to enumerate the address
book — for targeted lookups `search_contacts` is faster and cleaner:

```
hearth resource invoke my_contacts list_contacts
```

`list_contacts` takes no args, so omit the JSON object entirely.

Returns up to 100 contacts under `connections`, with a `nextPageToken` if
there are more. There is no pagination verb, so for a large address book
prefer `search_contacts` with a specific query.

## Response shapes — they differ, watch out

`search_contacts` wraps each hit in a `person` object:
```json
{
  "results": [
    {
      "person": {
        "resourceName": "people/c1234567890",
        "names": [{"displayName": "Dave Ruiz", "givenName": "Dave"}],
        "emailAddresses": [{"value": "dave@ruizplumbing.com"}],
        "phoneNumbers": [{"value": "+1 415 555 0100", "type": "mobile"}]
      }
    }
  ]
}
```

`list_contacts` returns a flat list under `connections`:
```json
{
  "connections": [
    {
      "resourceName": "people/c1234567890",
      "names": [{"displayName": "Dave Ruiz"}],
      "emailAddresses": [{"value": "dave@ruizplumbing.com"}]
    }
  ]
}
```

Fields are arrays because a person can have several emails or numbers; the
primary entry is index 0. Note that a contact may have **no** email at all —
plenty of household contacts are phone-only — so check before assuming
`emailAddresses[0]` exists.

## Finding someone's email for a calendar invite

The typical sequence when scheduling something:

```
# 1. Find the person
hearth resource invoke my_contacts search_contacts '{"query":"dave"}'

# 2. Take their address from results[0].person.emailAddresses[0].value
# 3. Use it to check availability, then create the event
hearth resource invoke family_calendar check_availability '{
  "time_min": "...",
  "time_max": "...",
  "items_json": "[{\"id\":\"dave@ruizplumbing.com\"}]"
}'
```

Note the calendar plugin's `items_json`: it ends in `_json`, so it is a
**string containing JSON**, escaped inside the outer object — not a nested
array.
