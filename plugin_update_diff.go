package main

import (
	"fmt"
	"sort"
	"strings"
)

// Detecting when a plugin update would break things that already work.
//
// The hazard is not obvious from looking at a manifest. IAM rules are keyed
// on the action string `external_resource.<plugin_slug>.<verb>`, so a verb
// that disappears — renamed, split, dropped — takes every rule naming it out
// of circulation. Nothing errors. The rules simply stop matching, and an
// agent that worked yesterday starts asking the phone for permission on
// every call with no explanation of why. The same is true of a connection's
// config: a version that adds a required key leaves existing connections
// quietly invalid until the next invoke fails.
//
// Both are silent, both are recoverable, and neither is visible at the moment
// you cause them. So the update refuses by default and says exactly what it
// would break, rather than applying and leaving someone to discover it.

// pluginUpdateDiff is what changes between two versions of a manifest.
type pluginUpdateDiff struct {
	VerbsAdded   []string
	VerbsRemoved []string
	// ConfigKeysNowRequired are config properties the new version requires
	// that the old one did not. Existing connections have no value for
	// these, so they are invalid the moment the update lands.
	ConfigKeysNowRequired []string

	// OrphanedRuleCount and AffectedAgents describe what the removal would
	// actually cost, fetched from the relay — the daemon knows which verbs
	// vanish but not which rules name them.
	//
	// Zero with Fetched=false means "we could not ask" (relay unreachable),
	// which is NOT the same as "nothing breaks" and must not be rendered as
	// reassurance.
	OrphanedRuleCount int
	AffectedAgents    []string
	ImpactFetched     bool
}

// Breaking reports whether applying this diff would invalidate something that
// currently works.
//
// Added verbs are NOT breaking: nothing references them yet. That asymmetry
// is the point — the common case (a plugin gaining capability) applies
// without ceremony, and only the cases that can silently break an agent
// interrupt anyone.
func (d pluginUpdateDiff) Breaking() bool {
	return len(d.VerbsRemoved) > 0 || len(d.ConfigKeysNowRequired) > 0
}

// Describe renders the breaking parts for a human. Empty when not breaking.
func (d pluginUpdateDiff) Describe(slug string) string {
	if !d.Breaking() {
		return ""
	}
	var parts []string
	if len(d.VerbsRemoved) > 0 {
		impact := ""
		switch {
		case d.ImpactFetched && d.OrphanedRuleCount > 0:
			impact = fmt.Sprintf(" — %d permission rule(s) will stop matching",
				d.OrphanedRuleCount)
			if len(d.AffectedAgents) > 0 {
				impact += ", affecting " + strings.Join(d.AffectedAgents, ", ")
			}
			impact += ", so those agents will start asking for approval again"
		case d.ImpactFetched:
			// A real zero is worth stating: nothing currently depends on
			// what is being removed.
			impact = " — no permission rules currently name them, so nothing loses access"
		default:
			// Could not find out. Say so rather than implying either outcome.
			//
			// Deliberately not "could not reach the server": a reachable
			// relay that refuses the request — an older one without the
			// handler — produces this same state, and blaming the network
			// would send someone debugging the wrong thing. The daemon logs
			// which of the two it was.
			impact = " — could not determine which permission rules name them, " +
				"so the impact is unknown"
		}
		parts = append(parts, fmt.Sprintf("removes %d verb(s): %s%s",
			len(d.VerbsRemoved), strings.Join(d.VerbsRemoved, ", "), impact))
	}
	if len(d.ConfigKeysNowRequired) > 0 {
		parts = append(parts, fmt.Sprintf(
			"newly requires config: %s — existing connections have no value for these and "+
				"will fail until reconfigured",
			strings.Join(d.ConfigKeysNowRequired, ", ")))
	}
	return fmt.Sprintf("updating %s %s", slug, strings.Join(parts, "; and it "))
}

// diffPluginManifests compares an installed manifest against an incoming one.
func diffPluginManifests(old, new PluginManifest) pluginUpdateDiff {
	oldVerbs := verbNameSet(old)
	newVerbs := verbNameSet(new)

	var d pluginUpdateDiff
	for name := range oldVerbs {
		if !newVerbs[name] {
			d.VerbsRemoved = append(d.VerbsRemoved, name)
		}
	}
	for name := range newVerbs {
		if !oldVerbs[name] {
			d.VerbsAdded = append(d.VerbsAdded, name)
		}
	}

	oldRequired := requiredConfigKeys(old)
	for _, key := range requiredConfigKeysSorted(new) {
		if !oldRequired[key] {
			d.ConfigKeysNowRequired = append(d.ConfigKeysNowRequired, key)
		}
	}

	// Sorted so the message is stable — map iteration order would otherwise
	// reshuffle the verb list on every run and make two identical refusals
	// look like different problems.
	sort.Strings(d.VerbsRemoved)
	sort.Strings(d.VerbsAdded)
	return d
}

func verbNameSet(m PluginManifest) map[string]bool {
	out := make(map[string]bool, len(m.Verbs))
	for _, v := range m.Verbs {
		if v.Name != "" {
			out[v.Name] = true
		}
	}
	return out
}

// requiredConfigKeys reads the `required` array out of a manifest's
// config_schema. Tolerant of every shape that is not a list of strings:
// config_schema is author-supplied JSON Schema, and a manifest we cannot
// fully understand must not become an install that refuses for a reason
// nobody can act on.
func requiredConfigKeys(m PluginManifest) map[string]bool {
	out := map[string]bool{}
	if m.ConfigSchema == nil {
		return out
	}
	raw, ok := m.ConfigSchema["required"]
	if !ok {
		return out
	}
	list, ok := raw.([]interface{})
	if !ok {
		return out
	}
	for _, item := range list {
		if s, ok := item.(string); ok && s != "" {
			out[s] = true
		}
	}
	return out
}

func requiredConfigKeysSorted(m PluginManifest) []string {
	set := requiredConfigKeys(m)
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
