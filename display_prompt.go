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
// {id,name}. Empty/absent → "" (no section, zero token cost) — a household with no
// screens, or an agent without display.publish, never sees it.
func buildDisplayPrompt(displaysJSON string) string {
	displaysJSON = strings.TrimSpace(displaysJSON)
	if displaysJSON == "" {
		return ""
	}
	var screens []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Access string `json:"access"`
	}
	if err := json.Unmarshal([]byte(displaysJSON), &screens); err != nil || len(screens) == 0 {
		return ""
	}

	// Split into screens you may publish to and personal screens you're only aware
	// of. An absent `access` means publishable (older server, shared screens).
	var publishable, awareness []string
	label := func(s struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Access string `json:"access"`
	}) string {
		if s.Name != "" {
			return s.Name
		}
		return s.ID
	}
	for _, s := range screens {
		if s.Access == "query" {
			awareness = append(awareness, label(s))
		} else {
			publishable = append(publishable, label(s))
		}
	}

	var b strings.Builder
	b.WriteString("## Household displays\n\n")
	if len(publishable) > 0 {
		b.WriteString("This household has screens you can put content on. To publish, run:\n\n")
		b.WriteString("  hearth display publish --target \"<screen>\" <url>\n")
		b.WriteString("  hearth display publish --target \"<screen>\" --type markdown --file <path>\n")
		b.WriteString("  hearth display publish --target \"<screen>\" --type html --file <path>\n\n")
		b.WriteString("`--target` takes the screen's name or id. `--type` may be url (default), image, video, markdown, or html. ")
		b.WriteString("Clear a screen with `hearth display clear --target \"<screen>\"`.\n\n")
		b.WriteString("A screen is a browser, so it can only load http(s) URLs — NEVER a local `file://` path; ")
		b.WriteString("that fails silently. To show your own content, deliver it INLINE from a file: `--type markdown --file` ")
		b.WriteString("for text, or `--type html --file` when you need styling markdown can't do (color, custom layout). ")
		b.WriteString("An image or video must be a real http(s) URL (`--type image`/`--type video`). ")
		b.WriteString("Custom-styled HTML renders sandboxed (no scripts), so use plain HTML + inline CSS.\n\n")
		b.WriteString("Screens differ in size and shape. Before you tune content for one, run ")
		b.WriteString("`hearth display query --target \"<screen>\"` — it reports what's showing and, when a ")
		b.WriteString("browser is connected, the screen's size (e.g. `1920×1080 px, 2x density (landscape)`). ")
		b.WriteString("Use it to tune font scale, layout density, image resolution, and aspect — but keep content ")
		b.WriteString("responsive: a screen may be a different size next time, and its size is unknown until a browser connects. ")
		b.WriteString("Screens you can publish to:\n\n")
		for _, name := range publishable {
			b.WriteString(fmt.Sprintf("- %s\n", name))
		}
	}
	if len(awareness) > 0 {
		if len(publishable) > 0 {
			b.WriteString("\n")
		}
		b.WriteString("These are personal screens you're aware of but may NOT publish to. You can ")
		b.WriteString("`hearth display query --target \"<screen>\"` them and mention them, but a publish will be ")
		b.WriteString("denied — the screen's owner would need to grant you access first. Don't attempt to publish; ")
		b.WriteString("instead tell the person that their own display is personal and they can grant access from the app.\n\n")
		for _, name := range awareness {
			b.WriteString(fmt.Sprintf("- %s (personal — awareness only)\n", name))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
