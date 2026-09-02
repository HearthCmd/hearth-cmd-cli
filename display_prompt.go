package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// buildDisplayPrompt renders the household-display competence section injected at
// spawn (CP4, docs/capability-provisioning.md §4). Displays aren't resource
// connections, so they have no plugin skill file; this spawn-time block is their
// durable "push=skill" competence — it teaches the `hearth display publish` verb
// and lists the screens this agent may publish to.
//
// displaysJSON is the server-computed spawn_context.displays: a JSON array of
// {id,name,access}. Empty/absent → "" (no section, zero token cost) — a household
// with no screens, or an agent without display.publish, never sees it.
//
// Screens are addressed BY ID, never by name. The ids are listed here, and
// `hearth display list` returns the live set on demand (which also picks up a
// screen added after spawn — this baked list is a starting point, not the last
// word). Publishing by id retires a whole class of failure: a homeowner names a
// screen "Laptop" and asks for "the laptop", and the agent no longer has to guess
// the exact spelling or casing — it lists, reads the id, and publishes to it.
func buildDisplayPrompt(displaysJSON string) string {
	displaysJSON = strings.TrimSpace(displaysJSON)
	if displaysJSON == "" {
		return ""
	}
	type screen struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Access string `json:"access"`
	}
	var screens []screen
	if err := json.Unmarshal([]byte(displaysJSON), &screens); err != nil || len(screens) == 0 {
		return ""
	}

	// Split into screens you may publish to and personal screens you're only aware
	// of. An absent `access` means publishable (older server, shared screens).
	line := func(s screen) string {
		name := s.Name
		if name == "" {
			name = "(unnamed)"
		}
		return fmt.Sprintf("- %s — `%s`", name, s.ID)
	}
	var publishable, awareness []string
	for _, s := range screens {
		if s.Access == "query" {
			awareness = append(awareness, line(s))
		} else {
			publishable = append(publishable, line(s))
		}
	}

	var b strings.Builder
	b.WriteString("## Household displays\n\n")
	b.WriteString("Screens are addressed by id, not name. The ids are below; run ")
	b.WriteString("`hearth display list` any time to see the current screens and their ids ")
	b.WriteString("(and to discover a screen added after you started — you can filter by name, ")
	b.WriteString("e.g. `hearth display list laptop`).\n\n")
	if len(publishable) > 0 {
		b.WriteString("You can put content on these screens. To publish, pass the screen's id to `--target`:\n\n")
		b.WriteString("  hearth display publish --target <id> <url>\n")
		b.WriteString("  hearth display publish --target <id> --type markdown --file <path>\n")
		b.WriteString("  hearth display publish --target <id> --type html --file <path>\n\n")
		b.WriteString("`--type` may be url (default), image, video, markdown, or html. ")
		b.WriteString("Clear a screen with `hearth display clear --target <id>`.\n\n")
		b.WriteString("A screen is a browser, so it can only load http(s) URLs — NEVER a local `file://` path; ")
		b.WriteString("that fails silently. To show your own content, deliver it INLINE from a file: `--type markdown --file` ")
		b.WriteString("for text, or `--type html --file` when you need styling markdown can't do (color, custom layout). ")
		b.WriteString("A raw image or video is served from its http(s) URL — hearth auto-detects `--type image`/`--type video` ")
		b.WriteString("from the file extension, but pass it explicitly for an extensionless URL (many image-CDN links have none). ")
		b.WriteString("The default `--type url` loads a page in an iframe, and some sites refuse to be embedded (they show blank) — ")
		b.WriteString("when in doubt, capture what you want to show as markdown or html and publish it inline. ")
		b.WriteString("Custom-styled HTML renders sandboxed (no scripts), so use plain HTML + inline CSS.\n\n")
		b.WriteString("Screens differ in size and shape. Before you tune content for one, run ")
		b.WriteString("`hearth display query --target <id>` — it reports what's showing and, when a ")
		b.WriteString("browser is connected, the screen's size (e.g. `1920×1080 px, 2x density (landscape)`). ")
		b.WriteString("Use it to tune font scale, layout density, image resolution, and aspect — but keep content ")
		b.WriteString("responsive: a screen may be a different size next time, and its size is unknown until a browser connects. ")
		b.WriteString("Screens you can publish to:\n\n")
		b.WriteString(strings.Join(publishable, "\n"))
		b.WriteString("\n")
	}
	if len(awareness) > 0 {
		if len(publishable) > 0 {
			b.WriteString("\n")
		}
		b.WriteString("These are personal screens you're aware of but may NOT publish to. You can ")
		b.WriteString("`hearth display query --target <id>` them and mention them, but a publish will be ")
		b.WriteString("denied — the screen's owner would need to grant you access first. Don't attempt to publish; ")
		b.WriteString("instead tell the person that their own display is personal and they can grant access from the app.\n\n")
		for _, l := range awareness {
			b.WriteString(l + " (personal — awareness only)\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
