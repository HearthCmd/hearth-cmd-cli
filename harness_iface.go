package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Harness abstracts what the daemon needs to know about each agent CLI
// (claude, codex, copilot, gemini, pi). Replaces the
// `switch agent {}` pattern scattered across the daemon — see
// docs/forward-compat-harness-interface.md and docs/iam-planning.md
// (refactor #4).
//
// The interface has grown incrementally as harnesses were ported:
//   - Spawn-side: Name/Binary/ServerName/SupportsResume, Argv,
//     TranscriptPath, SubmitDelay, PostSubmit, PreSpawn,
//     EnsureHelperEntitlements, SessionIDPolicy, AssignedSessionID,
//     NeedsInjectGate, WarmupPayload, ModelEnv, ReportsResumeID.
//   - Stream-side: NewStreamTransformer (replaces the per-harness
//     stream*Bridge fan-out and daemon_history.go's historyTransforms).
//   - Versioning: MinimumVersion, KnownTestedVersions (gated by
//     ProbeAllHarnessVersions at startup + CheckSpawnPreconditions at
//     spawn).
//
// Per-harness implementations live next to this file:
// harness_claude.go, harness_pi.go, etc. Each registers itself in an
// init() so daemon core never imports a specific harness type.
type Harness interface {
	// Name is the local identifier the daemon and CLI use everywhere
	// (e.g. "claude"). Stable across the codebase.
	Name() string

	// Binary is the executable name we look up on PATH when spawning.
	// Often equals Name(); rare for them to differ.
	Binary() string

	// ServerName is the value stored in the server's harnesses.name
	// column for this harness (e.g. "claude-code" for claude). Used for
	// translation between server-side and local-side identifiers; the
	// inverse direction is getHarnessByServerName.
	ServerName() string

	// SupportsResume reports whether the underlying CLI advertises a
	// resume affordance to the user. Note: this is about *user-facing*
	// resume UX, not internal session-file handling — pi reads --session
	// but still returns false here because users don't drive that flow.
	SupportsResume() bool

	// Argv returns harness-specific flags to append after the binary
	// name. Caller-built systemPrompt and any session id arrive via ctx.
	// Returning an empty slice is fine for harnesses with no extra args.
	Argv(ctx HarnessCtx) []string

	// TranscriptPath returns the absolute path where the harness writes
	// its session transcript JSONL (or whatever per-harness format). May
	// return "" when the path can't be determined yet (file not created,
	// session id unknown, etc.); callers poll until it appears.
	TranscriptPath(ctx HarnessCtx) string

	// SubmitDelay is the per-harness pause between injecting paste
	// content and sending the \r submit byte. Most harnesses are happy
	// with ~50ms; gemini's TextInput needs ~300ms. The daemon's relay
	// path applies this between paste payload and submit.
	SubmitDelay() time.Duration

	// PostSubmit runs immediately after the \r submit byte is injected,
	// with the process handle of the child harness. Most harnesses are
	// no-ops (return nil); gemini sends SIGWINCH to flush its
	// TextInput's paste-buffering. Errors are logged, never fatal.
	PostSubmit(proc *os.Process) error

	// PreSpawn runs once before the binary is exec'd. Idempotent. The
	// place for per-harness on-disk setup that has to happen before the
	// agent starts: trust-dialog pre-accepts (claude, copilot),
	// project-local settings seeds (claude bypassPermissions), and
	// instruction-file installs (gemini GEMINI.md, copilot
	// .github/copilot-instructions.md, codex AGENTS.md). Errors are
	// logged by the caller, never fatal — best-effort, just like the
	// pre-SPI helpers it absorbs.
	PreSpawn(ctx HarnessCtx) error

	// EnsureHelperEntitlements runs on darwin only, after the agent's
	// own binary has been entitled and right before exec, to re-sign
	// any helper / child binaries that need to inherit
	// DYLD_INSERT_LIBRARIES (copilot's spawn-helper, codex's vendored
	// Rust binary). Most harnesses don't have helper binaries and
	// return nil. Errors are logged inside, never returned fatally —
	// matches the existing best-effort contract.
	EnsureHelperEntitlements()

	// SessionIDPolicy describes how the daemon should derive this
	// harness's agent-session id at spawn time. See SessionIDPolicy
	// constants.
	SessionIDPolicy() SessionIDPolicy

	// AssignedSessionID is the inverse of "the daemon assigned an id":
	// for SessionIDHarnessAssigned harnesses (codex), the binary picks
	// its own UUID and embeds it in the transcript filename. After the
	// streamer discovers the file, it calls this to recover that UUID
	// and report it back via reportLastSessionID so a future wake can
	// pass it as `resume <UUID>`. Returns "" for harnesses where the
	// daemon already knows the id (SessionIDMint) or there is no id
	// (SessionIDNone).
	AssignedSessionID(transcriptPath string) string

	// NeedsInjectGate reports whether the daemon should hold the first
	// inject until the child has confirmed bracketed-paste mode (by
	// emitting \x1b[?2004h) or 1.5s of quiet output, whichever comes
	// first. Used to dodge the "first user message gets eaten because
	// the TUI hadn't enabled bracketed paste yet" failure mode that
	// codex and gemini both exhibit. False for everyone else.
	NeedsInjectGate() bool

	// SupportsAttach reports whether `hearth hh agent attach` can tee
	// onto this harness's PTY without breaking it. When true the
	// daemon stands up the per-instance unix socket at spawn time;
	// when false the CLI surfaces a "agent not running on this host"
	// error.
	//
	// The bar is "PTY output passes through cleanly + InjectRaw
	// (write-mode) doesn't trip the harness's input loop." Most
	// harnesses pass after a smoke test; some have warm-up quirks
	// (codex bracketed-paste, gemini SIGWINCH-after-\\r) that may
	// need bespoke handling before this can flip true. Default
	// false on a new adapter — opt in once validated.
	SupportsAttach() bool

	// MinimumVersion is the lowest version of the underlying binary
	// this adapter is known to work with. Hard floor: the daemon
	// refuses to spawn if the detected version is below this, with a
	// clear "please upgrade" error.
	//
	// **Policy:** floor is pinned to the highest validated version
	// (== highest entry in KnownTestedVersions). Bump in lockstep
	// when you add to the tested set. See
	// memory/project_harness_version_gate.md for the validation flow.
	//
	// When multiple adapters register for the same harness name
	// (future work), each adapter's MinimumVersion (and an implicit
	// upper bound at the next adapter's Min) is what the registry
	// uses to pick which one handles a given installed version.
	MinimumVersion() string

	// KnownTestedVersions is the set of versions this adapter has
	// been validated against end-to-end on the dev box. Drift OUTSIDE
	// this list (but above MinimumVersion) produces a warning log
	// at startup but doesn't block — silent version creep is the
	// failure mode we're solving; a warning lets the user notice
	// without us having to update the list for every harness release.
	//
	// **To add a version:** smoke-test end-to-end through hearth
	// (spawn, message, response, transcript, tool call, sleep/wake/
	// resume), then append the version string here AND bump
	// MinimumVersion to match. Full procedure in
	// memory/project_harness_version_gate.md.
	//
	// Order doesn't matter; semantically a set.
	KnownTestedVersions() []string

	// ModelEnv returns the (env-var name, value) pair the daemon should
	// set when modelName is non-empty, telling this harness which AI
	// brain model to use. Returns ("", "", false) for harnesses that
	// configure their model inside their own UI (gemini, copilot, pi)
	// — modelName is informational only for those. Claude reads
	// ANTHROPIC_MODEL; codex reads OPENAI_MODEL.
	ModelEnv(modelName string) (key, value string, ok bool)

	// WarmupPayload returns bytes to write to the PTY when the inject
	// gate opens, ahead of any user injects. Returns nil for harnesses
	// that don't need a warmup turn. Used by codex: codex 0.128 only
	// flushes its rollout JSONL on the SECOND user turn, so the daemon
	// primes it with a `<hearth-warmup>` message that AGENTS.md tells
	// codex to ignore. NeedsInjectGate must return true when
	// WarmupPayload is non-nil — the gate's open signal is what drives
	// the warmup write.
	WarmupPayload() []byte

	// NewStreamTransformer returns a fresh per-spawn transformer that
	// converts this harness's on-disk transcript JSONL into bridge-shape
	// lines (the claude-compatible JSON the server expects). Returning a
	// new instance per call lets the transformer carry stateful context
	// across lines from the same file — gemini extracts its sessionId
	// from the header line and reuses it on every following line; other
	// harnesses' transformers are effectively stateless.
	//
	// The single `tailAndPump` loop in stream.go calls TransformLine for
	// every line read from the transcript file. The history-replay path
	// in daemon_history.go does the same, with a freshly constructed
	// transformer per replay.
	NewStreamTransformer() StreamTransformer

	// ReportsResumeID indicates whether the daemon should call
	// reportLastSessionID after the harness picks up an AgentSessionID,
	// so the next wake can hand it back via spawn_context.last_session_id.
	// True for harnesses where the server's last_session_id is the key
	// the next wake uses to reattach (claude, copilot, pi). False for
	// harnesses that resolve sessions differently at wake time (codex
	// via cwd lookup; gemini reads its session id out of the JSONL
	// header). Independent of SessionIDPolicy on principle — a harness
	// could mint without reporting, or reuse-instance-id and still
	// report — even though all five current harnesses pair the two
	// the same way.
	ReportsResumeID() bool

	// InstallSkill makes a resource plugin's skill content available to
	// the agent in the harness-native way. Called once per active
	// resource binding at spawn time, after PreSpawn. skillContent is
	// the raw bytes of the plugin's skill.md (Claude YAML frontmatter +
	// markdown body). connectionID and pluginSlug scope the installed
	// artifact so multiple bindings don't collide.
	//
	// Claude writes to <cwd>/.claude/skills/<pluginSlug>-<connectionID>/SKILL.md
	// so Claude Code's native progressive-loading picks it up.
	// Codex/Gemini/Copilot append the skill body inline to their
	// existing instruction file (AGENTS.md / GEMINI.md /
	// copilot-instructions.md). Harnesses with no suitable injection
	// point return nil without doing anything.
	//
	// Errors are logged by the caller, never fatal — same contract as
	// PreSpawn.
	InstallSkill(ctx HarnessCtx, connectionID, pluginSlug string, skillContent []byte) error

	// RemoveSkill undoes InstallSkill for the given connection, called
	// at spawn time for connections that are no longer granted to the
	// agent (reconcile pass). Mirrors the installation path:
	//
	// Claude removes <cwd>/.claude/skills/<pluginSlug>-<connectionID>/.
	// Codex/Gemini/Copilot strip the <!-- hearth-skill:<connectionID> -->
	// block from the instruction file. Idempotent — no-op if not installed.
	//
	// Errors are logged by the caller, never fatal.
	RemoveSkill(ctx HarnessCtx, connectionID, pluginSlug string) error
}

// StreamTransformer converts on-disk transcript JSONL lines into
// bridge-shape lines (the claude-compatible JSON the server expects).
// Returned by Harness.NewStreamTransformer(); one instance per spawn so
// implementations can keep cross-line state (e.g. gemini's per-file
// sessionID). Returning a zero-length slice means "this line produced
// nothing renderable" (e.g. a $set tick, a slash-command, an event
// type the bridge doesn't surface).
type StreamTransformer interface {
	TransformLine(line string) [][]byte
}

// SessionIDPolicy tags how the daemon derives a harness's
// agent-session id at spawn. The enum is small; new values get added
// only when a new harness can't be classified into an existing one.
type SessionIDPolicy int

const (
	// SessionIDNone: the harness doesn't take a session id at spawn.
	// AgentSessionID stays "" through the spawn path. Used by gemini —
	// it stores its session id in the JSONL header at write time and
	// we resolve it by scanning afterwards.
	SessionIDNone SessionIDPolicy = iota

	// SessionIDMint: daemon mints a fresh UUID at spawn time (or carries
	// forward last_session_id when priorSessionUsable). Used by claude,
	// copilot, pi.
	SessionIDMint

	// SessionIDHarnessAssigned: the harness binary picks its own UUID
	// at first turn and embeds it in the transcript filename — the
	// daemon never mints. The daemon still carries forward
	// last_session_id from a prior run when priorSessionUsable, and
	// the harness's Argv passes it as a resume arg (e.g. codex's
	// `resume <UUID>` subcommand). Used by codex.
	SessionIDHarnessAssigned
)

// HarnessCtx carries the spawn-time and stream-time values per-harness
// methods need. New fields are added as the interface grows; harness
// implementations only read the fields they care about, so additions
// never break existing impls.
type HarnessCtx struct {
	// AgentSessionID is the harness-internal session id selected by the
	// caller for this spawn (or carried forward from a prior spawn when
	// resuming). May be "" for harnesses or codepaths that don't use it.
	AgentSessionID string

	// ResumingPriorSession is true when AgentSessionID came from a prior
	// spawn whose on-disk transcript still exists and we want the
	// harness to reattach. Drives flag selection (claude --resume vs
	// --session-id; copilot --resume; pi --session).
	ResumingPriorSession bool

	// SystemPrompt is the composed identity-prompt + hearth permission
	// instructions. Harnesses that accept --append-system-prompt pass
	// this verbatim; others ignore it (their PreSpawn writes the
	// instruction-file equivalent to disk).
	SystemPrompt string

	// IdentityPrompt is the bare identity paragraph (no hearth
	// instructions appended) — what installHearthInstructions wants as
	// its instruction-file body. SystemPrompt has the same content
	// composed in; keep both to avoid forcing harnesses that need only
	// one to do the split themselves.
	IdentityPrompt string

	// AIAgentInstanceID is the server-side agent instance id. Surfaced
	// for harnesses whose on-disk setup embeds it (codex's AGENTS.md
	// sentinel). Most harnesses ignore it.
	AIAgentInstanceID string

	// Cwd is the agent's working directory — the path the harness
	// derives its on-disk transcript location from.
	Cwd string
}

// harnessRegistry holds every Harness keyed by Name(). The slice value
// (always length 1 today) leaves room for future per-version adapters:
// when one codex/gemini/etc. behavior diverges enough that branching
// inside a single adapter is unreadable, a second adapter can register
// under the same name with a different MinimumVersion. The registry
// then picks the adapter whose range covers the detected installed
// version. Populated by init() in each per-harness file; read-mostly,
// no locking needed because all writes happen during init().
var harnessRegistry = map[string][]Harness{}

// versionProbeRegistry holds the version-detection function per
// harness name. Separated from the adapter registry on purpose: when
// multiple adapters share a name (future), they all want the same
// version probe (it's a property of the *binary*, not of which adapter
// is chosen). One probe per harness name.
var versionProbeRegistry = map[string]VersionProbe{}

// versionCache memoizes the detected version of each harness binary.
// Populated by ProbeAllHarnessVersions() at daemon startup; lookups
// read from here so per-spawn paths don't re-shell-out. Entries map
// harness name → detected semver string; an entry with value "" means
// the probe ran but couldn't determine a version (binary not present,
// parse failed, etc.) — distinct from "not yet probed" (no entry).
var (
	versionCacheMu sync.RWMutex
	versionCache   = map[string]string{}
)

// VersionProbe runs the harness binary and parses its version output.
// Returns a normalized semver-ish string ("0.128.0", "1.0.0", etc.)
// or "" + error if the binary isn't on PATH / doesn't run / output
// can't be parsed. Each harness owns its own probe since `--version`
// output shapes differ (claude prints "1.0.0 (Claude Code)", codex
// prints "codex-cli 0.128.0", gemini prints bare "0.40.1", etc.).
type VersionProbe func() (string, error)

// registerHarness adds h to the registry under its Name(). Today every
// adapter under a given name is the only one, so the slice always has
// length 1. Multiple adapters per name are tolerated for forward-compat
// but selection logic is unchanged until callers want it.
func registerHarness(h Harness) {
	name := h.Name()
	harnessRegistry[name] = append(harnessRegistry[name], h)
}

// registerVersionProbe associates a version-detection function with a
// harness name. Panics on duplicate — there should be exactly one
// probe per harness, set in the harness's init().
func registerVersionProbe(name string, probe VersionProbe) {
	if _, ok := versionProbeRegistry[name]; ok {
		panic("version probe already registered: " + name)
	}
	versionProbeRegistry[name] = probe
}

// getHarness looks up a Harness by local name and picks the adapter
// whose MinimumVersion <= the detected installed version. Today each
// name maps to a single adapter so the version check is trivially
// satisfied; the structure leaves room for multiple adapters per name.
// Returns (nil, false) if no adapter is registered OR none of the
// registered adapters' MinimumVersion floors are met by the detected
// version. A registered-but-unprobed harness (entry absent from
// versionCache) is treated as "version OK" — the at-spawn enforcement
// path handles the refusal with a clearer error.
func getHarness(name string) (Harness, bool) {
	adapters, ok := harnessRegistry[name]
	if !ok || len(adapters) == 0 {
		return nil, false
	}
	installed := getCachedVersion(name)
	for _, h := range adapters {
		if installed == "" || semverGTE(installed, h.MinimumVersion()) {
			return h, true
		}
	}
	return nil, false
}

// getHarnessByServerName is the inverse of Harness.ServerName(). Same
// version-aware selection as getHarness; linear scan over the small
// registry (called rarely, only on the server-harness → local-agent
// translation path).
func getHarnessByServerName(serverName string) (Harness, bool) {
	for _, adapters := range harnessRegistry {
		installed := ""
		if len(adapters) > 0 {
			installed = getCachedVersion(adapters[0].Name())
		}
		for _, h := range adapters {
			if h.ServerName() != serverName {
				continue
			}
			if installed == "" || semverGTE(installed, h.MinimumVersion()) {
				return h, true
			}
		}
	}
	return nil, false
}

// getCachedVersion returns the detected installed version of the
// named harness, or "" if not yet probed / probe failed.
func getCachedVersion(name string) string {
	versionCacheMu.RLock()
	defer versionCacheMu.RUnlock()
	return versionCache[name]
}

// setCachedVersion stores a probe result. Called by the daemon's
// startup probe pass.
func setCachedVersion(name, version string) {
	versionCacheMu.Lock()
	defer versionCacheMu.Unlock()
	versionCache[name] = version
}

// ProbeAllHarnessVersions runs every registered version probe and
// stores the result in versionCache. Idempotent; safe to call more
// than once if e.g. the user upgrades a harness mid-run and reloads.
// Logs each probe's outcome once so the rest of the daemon can stay
// quiet about versions during normal operation:
//   - probe failed (binary absent etc.): one log, cache "".
//   - version detected and in KnownTestedVersions: one info log.
//   - version detected but NOT in KnownTestedVersions (and the
//     adapter has a non-empty list): one WARN log — this is the
//     "silent version creep" signal we want to surface.
//   - adapter declares no tested list: one info log noting that.
func ProbeAllHarnessVersions() {
	for name, probe := range versionProbeRegistry {
		v, err := probe()
		if err != nil {
			log.Printf("harness version probe failed for %s: %v", name, err)
			setCachedVersion(name, "")
			continue
		}
		setCachedVersion(name, v)
		h, ok := getHarness(name)
		if !ok {
			log.Printf("harness %s version detected: %s (no adapter registered?)", name, v)
			continue
		}
		tested := h.KnownTestedVersions()
		if len(tested) == 0 {
			log.Printf("harness %s version detected: %s (no tested-version set declared)", name, v)
			continue
		}
		matched := false
		for _, t := range tested {
			if t == v {
				matched = true
				break
			}
		}
		if matched {
			log.Printf("harness %s version detected: %s (tested)", name, v)
		} else {
			log.Printf("WARN: harness %s version %s is not in the validated set %v; proceed at your own risk", name, v, tested)
		}
	}
}

// CheckSpawnPreconditions enforces the version gate at spawn time.
// Today: hard refuse only — returns an error if the detected version
// is below the adapter's MinimumVersion, nil otherwise. The "untested
// version" warning is logged once at startup (see
// ProbeAllHarnessVersions) rather than at every spawn.
func CheckSpawnPreconditions(h Harness) error {
	name := h.Name()
	installed := getCachedVersion(name)
	if installed == "" {
		// Probe didn't fire or failed. Don't block — the binary
		// might still work; the absence of a detected version was
		// already logged at startup.
		return nil
	}
	if min := h.MinimumVersion(); min != "" && !semverGTE(installed, min) {
		return fmt.Errorf("harness %s: installed version %s is below the minimum supported %s; please upgrade", name, installed, min)
	}
	return nil
}

// semverGTE reports whether installed >= floor under a relaxed semver
// ordering: numeric component-wise compare, missing components treated
// as 0, non-numeric suffixes ignored after the third component. "0.128"
// and "0.128.0" compare equal; "0.128.1" > "0.128.0"; "1.0.0" > "0.99.0".
// Returns false if either string is empty or contains non-digit/non-dot
// chars in the leading components (defensive — we'd rather refuse than
// silently miscompare).
func semverGTE(installed, floor string) bool {
	if floor == "" {
		return true
	}
	if installed == "" {
		return false
	}
	a := semverParts(installed)
	b := semverParts(floor)
	if a == nil || b == nil {
		return false
	}
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

// semverParts splits "1.2.3"-style into three ints. Pads missing
// components with 0. Returns nil on parse failure of the first three
// components; trailing junk (e.g. "1.2.3-rc1") is tolerated by parsing
// the third component up to the first non-digit.
func semverParts(s string) []int {
	parts := strings.SplitN(s, ".", 4)
	out := []int{0, 0, 0}
	for i := 0; i < 3 && i < len(parts); i++ {
		p := parts[i]
		end := len(p)
		for j, r := range p {
			if r < '0' || r > '9' {
				end = j
				break
			}
		}
		if end == 0 {
			return nil
		}
		n, err := strconv.Atoi(p[:end])
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}

// semverRe matches the first <digits>.<digits>[.<digits>] substring
// in a string. Used by per-harness version probes that share the same
// "run --version, find the first semver-looking thing" parse strategy.
// Anchored on word boundaries so we don't grab a fragment of a larger
// number; intentionally loose on the third component so "1.0" parses
// as "1.0.0".
var semverRe = regexp.MustCompile(`\b(\d+)\.(\d+)(?:\.(\d+))?\b`)

// runVersionCommand is the shared helper most harness probes use:
// runs `<binary> <args...>` (typically `--version`), captures combined
// output, and returns the first semver-shaped substring normalized to
// MAJOR.MINOR.PATCH (filling a missing PATCH with 0). Returns "" +
// error if the binary can't be found, the command fails, or no semver
// pattern appears in the output.
func runVersionCommand(binary string, args []string) (string, error) {
	if _, err := exec.LookPath(binary); err != nil {
		return "", fmt.Errorf("binary %q not on PATH: %w", binary, err)
	}
	cmd := exec.Command(binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("running %s %v: %w (output: %q)", binary, args, err, trimForLog(out))
	}
	return extractFirstSemver(string(out))
}

// extractFirstSemver pulls the first semver-shaped substring from
// text. Exposed for harnesses whose version printout isn't reachable
// via `--version` (e.g. a wrapper that prints to stderr in a custom
// shape); most harnesses use runVersionCommand instead.
func extractFirstSemver(text string) (string, error) {
	m := semverRe.FindStringSubmatch(text)
	if m == nil {
		return "", fmt.Errorf("no semver substring found in output: %q", trimForLog([]byte(text)))
	}
	patch := m[3]
	if patch == "" {
		patch = "0"
	}
	return m[1] + "." + m[2] + "." + patch, nil
}

// trimForLog returns a single-line, length-capped form of `out` so
// error messages with command output don't blow up logs.
func trimForLog(out []byte) string {
	s := strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
