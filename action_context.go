//go:build darwin || linux

package main

import "net/http"

// X-Hearth-Action wiring. Spec:
// hearth-cmd/docs/forward-compat-resource-scope-tuple.md.
//
// Every authenticated outbound request declares its (resource, action)
// tuple in the X-Hearth-Action header. Today the server logs the
// parsed tuple; future IAM enforcement plumbs in there. We carry it
// from day one so adding policy enforcement later doesn't require
// touching every call site.

// ActionTuple is the (resource_kind, resource_id, action) tuple. id
// may be empty for collection-scoped actions (e.g. "list", "create").
type ActionTuple struct {
	Kind   string
	ID     string
	Action string
}

func (a ActionTuple) header() string {
	return a.Kind + ":" + a.ID + ":" + a.Action
}

func addActionHeader(req *http.Request, a ActionTuple) {
	req.Header.Set("X-Hearth-Action", a.header())
}
