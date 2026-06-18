//go:build darwin || linux

package main

import (
	"fmt"
	"sort"
	"strings"
)

// buildResourcePluginPrompt renders the per-verb detail block the
// agent's system prompt advertises, filtered to connections this
// specific agent has been granted access to. The shape is:
//
//	You can invoke external resources via:
//	  hearth resource invoke <connection-id> <verb> [args-json]
//
//	Available resources:
//
//	  ha-home (Home Assistant)
//	    turn_on   — Turn a switchable entity on.
//	                args: { "entity_id": "string (required)" }
//	                output: json
//	    ...
//
// Returns the empty string when the agent has no granted connections,
// so non-plugin agents pay zero token cost. The block is injected by
// the agent-spawn path (agent_setup.go), prepended to the
// hearthSystemPrompt.
//
// agentID + grants gate the filter: a connection only appears if
// grants.HasGrant(agentID, conn.ConnectionID) is true. Empty agentID
// or nil grants store renders nothing — agents without identity
// can't have grants and shouldn't see any resources.
//
// entitiesByConn (optional, may be nil) supplies the per-connection
// entity caches for adapters that have one. When present, each
// connection's block includes a list of available entity_ids with
// their kind + a short label projection, capped at maxEntitiesPerConn
// with a "...and N more" line. Empty / missing entities for a given
// connection render no Entities section (omitted cleanly).
func buildResourcePluginPrompt(agentID string, grants *AgentGrantsStore, store *ResourceConnectionStore, plugins *PluginRegistry, entitiesByConn map[string][]Entity) string {
	if agentID == "" || grants == nil || store == nil || plugins == nil {
		return ""
	}
	conns := store.List()
	if len(conns) == 0 {
		return ""
	}
	// Filter to granted-only before rendering. Drops the rest of the
	// host's connections from the agent's view; this is the IAM-driven
	// per-agent visibility gate.
	granted := make([]ResourceConnection, 0, len(conns))
	for _, c := range conns {
		if grants.HasGrant(agentID, c.ConnectionID) {
			granted = append(granted, c)
		}
	}
	if len(granted) == 0 {
		return ""
	}
	conns = granted
	sort.Slice(conns, func(i, j int) bool {
		return conns[i].ConnectionID < conns[j].ConnectionID
	})

	var b strings.Builder
	b.WriteString("You can invoke external resources via:\n")
	b.WriteString("  hearth resource invoke <connection-id> <verb> [args-json] [--secret <env>=<id>]\n\n")
	b.WriteString("Resource invokes are authorized separately from Bash tool calls ")
	b.WriteString("and have their own audit log. Errors come back as ")
	b.WriteString(`"hearth resource: <code>: <message>" on stderr.`)
	b.WriteString("\n\n")
	b.WriteString("When a verb needs a credential (declared per-connection below), look up its ")
	b.WriteString("secret id with `hearth secret list` and pass `--secret <env>=<id>` on the invoke. ")
	b.WriteString("The <env> name matches the credential name in the per-connection Credentials block.\n\n")
	b.WriteString("Available resources:\n")

	rendered := 0
	for _, c := range conns {
		m, ok := plugins.GetPluginBySlug(c.PluginSlug)
		if !ok {
			continue
		}
		label := c.Slug
		if label == "" {
			label = c.ConnectionID
		}
		fmt.Fprintf(&b, "\n  %s (%s)\n", label, m.DisplayName)
		if len(m.Verbs) == 0 {
			b.WriteString("    (no verbs declared)\n")
			rendered++
			continue
		}
		// Stable ordering across spawns so caches/diffs are clean.
		verbs := make([]PluginVerb, len(m.Verbs))
		copy(verbs, m.Verbs)
		sort.Slice(verbs, func(i, j int) bool { return verbs[i].Name < verbs[j].Name })
		b.WriteString("    Verbs:\n")
		for _, v := range verbs {
			writeVerbBlock(&b, v)
		}
		writeCredentialsBlock(&b, m.Credentials)
		writeEntitiesBlock(&b, entitiesByConn[c.ConnectionID])
		rendered++
	}
	if rendered == 0 {
		// All connections referenced unregistered plugins. Don't ship
		// a header with no body; that's just confusing.
		return ""
	}
	return b.String()
}

// maxEntitiesPerConn caps the per-connection entity list in the
// agent's system prompt. Households with hundreds of HA entities
// would otherwise dominate the prompt; an agent that needs the full
// set can call `list_entities` (or its adapter's equivalent).
const maxEntitiesPerConn = 50

func writeVerbBlock(b *strings.Builder, v PluginVerb) {
	desc := v.Description
	if desc == "" {
		desc = "(no description)"
	}
	fmt.Fprintf(b, "      %s — %s\n", v.Name, firstLine(desc))
	if len(v.Args) > 0 {
		b.WriteString("                args: { ")
		// Sort arg names so the rendered block is stable.
		keys := make([]string, 0, len(v.Args))
		for k := range v.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			spec := v.Args[k]
			ty := spec.Type
			if ty == "" {
				ty = "any"
			}
			if spec.Required {
				ty += " (required)"
			}
			parts = append(parts, fmt.Sprintf("%q: %q", k, ty))
		}
		b.WriteString(strings.Join(parts, ", "))
		b.WriteString(" }\n")
	}
	if v.Output != "" {
		fmt.Fprintf(b, "                output: %s\n", v.Output)
	}
}

// writeCredentialsBlock renders the per-connection list of credentials
// the manifest declares so the agent knows what `--secret <env>=<id>`
// names to pass. Omitted when the manifest declares no credentials.
// The secret id itself isn't emitted here (it's per-host operator
// state, fetched via `hearth secret list`); the prompt's preamble
// covers that workflow.
func writeCredentialsBlock(b *strings.Builder, creds []PluginCredential) {
	if len(creds) == 0 {
		return
	}
	b.WriteString("    Credentials:\n")
	for _, c := range creds {
		desc := firstLine(c.Description)
		if desc == "" {
			desc = "(no description)"
		}
		secretMark := ""
		if c.Secret {
			secretMark = " (secret)"
		}
		fmt.Fprintf(b, "      %s%s — %s\n", c.Name, secretMark, desc)
	}
}

// writeEntitiesBlock lists the daemon's cached entities for this
// connection, capped at maxEntitiesPerConn. Each line is the
// entity_id followed by kind and any labels in parens, so an agent
// can tell at a glance what's available without calling list_entities.
// Empty / nil entities render nothing (cleanly omitted).
func writeEntitiesBlock(b *strings.Builder, entities []Entity) {
	if len(entities) == 0 {
		return
	}
	// Stable ordering: sort by entity_id so spawns are byte-identical
	// across runs with the same cache (helps prompt caches; helps
	// transcript diffs).
	ents := make([]Entity, len(entities))
	copy(ents, entities)
	sort.Slice(ents, func(i, j int) bool { return ents[i].EntityID < ents[j].EntityID })

	b.WriteString("    Entities:\n")
	n := len(ents)
	if n > maxEntitiesPerConn {
		n = maxEntitiesPerConn
	}
	for i := 0; i < n; i++ {
		e := ents[i]
		fmt.Fprintf(b, "      %s  (kind=%s", e.EntityID, e.Kind)
		if len(e.Labels) > 0 {
			// Stable label order.
			keys := make([]string, 0, len(e.Labels))
			for k := range e.Labels {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(b, ", %s=%s", k, e.Labels[k])
			}
		}
		if e.Parent != "" {
			fmt.Fprintf(b, ", parent=%s", e.Parent)
		}
		b.WriteString(")\n")
	}
	if len(ents) > maxEntitiesPerConn {
		fmt.Fprintf(b, "      ...and %d more (call list_entities or the adapter's listing verb to see all)\n",
			len(ents)-maxEntitiesPerConn)
	}
}

// firstLine collapses a multi-line description to the first line —
// manifest authors sometimes use YAML block scalars (|) for verb
// descriptions, but the agent's prompt benefits from a one-liner.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
