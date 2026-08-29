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
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(displaysJSON), &screens); err != nil || len(screens) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Household displays\n\n")
	b.WriteString("This household has screens you can put content on. To publish, run:\n\n")
	b.WriteString("  hearth display publish --target \"<screen>\" <url>\n")
	b.WriteString("  hearth display publish --target \"<screen>\" --type markdown --file <path>\n\n")
	b.WriteString("`--target` takes the screen's name or id. `--type` may be url (default), image, video, or markdown. ")
	b.WriteString("Clear a screen with `hearth display clear --target \"<screen>\"`. Screens available to you:\n\n")
	for _, s := range screens {
		if s.Name != "" {
			b.WriteString(fmt.Sprintf("- %s\n", s.Name))
		} else {
			b.WriteString(fmt.Sprintf("- %s\n", s.ID))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
