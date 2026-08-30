package main

import "strings"

// buildPresencePrompt renders the voice-presence competence section injected at
// spawn (local/presence rung LP2, docs/local-presence-rung-plan.md). It teaches
// the "someone at a shared device identifies themselves → fast-lane their phone"
// pattern. Static (no server data), and shares buildDisplayPrompt's composition
// slot. Always present, but zero behavioral cost for an agent never driven by a
// shared voice device — it simply never has cause to use it.
func buildPresencePrompt() string {
	var b strings.Builder
	b.WriteString("## Approvals for someone at a shared device\n\n")
	b.WriteString("When you're spoken to through a shared voice device (a puck) and the person ")
	b.WriteString("identifies themselves — \"I'm Matt\", \"this is Sarah\" — you can fast-lane ")
	b.WriteString("approvals to THEM instead of paging the whole household:\n\n")
	b.WriteString("1. Find their member id: `hearth hh user list`, matching the spoken name. If two ")
	b.WriteString("members share a name, ask which one; if nobody matches, say so and carry on.\n")
	b.WriteString("2. Assert their presence: `hearth presence assert --human <id>`.\n\n")
	b.WriteString("Afterwards, any action this session that needs approval pages that person's phone ")
	b.WriteString("first, and they approve with their own biometric — saying a name never approves ")
	b.WriteString("anything by itself. If the command reports they're not set up to approve here, ")
	b.WriteString("tell them, and an authorized approver will be asked instead. Only assert presence ")
	b.WriteString("for someone who actually just identified themselves out loud — never guess who's there.")
	return b.String()
}
