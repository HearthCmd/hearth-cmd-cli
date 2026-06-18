# Resource plugins — sub-phase 1a implementation plan

**Status:** planning. Branch `resource-plugins` (sibling of `main`, no
new commits). Authoritative architecture doc:
`hearth-cmd/docs/external-resource-adapters.md`. Phase 0 (IAM
checkRules generalization) shipped to hearth-cmd's resource-plugins
branch 2026-05-14.

**Goal of 1a:** smallest self-contained chunk of the plugin
substrate — plugin discovery + manifest parsing + in-memory registry.
No subprocess lifecycle, no RPC, no CLI subcommand, no IAM
integration. Just "daemon boots → reads
`~/.hearth/plugins/*/manifest.yaml` → exposes parsed manifests
through a registry that later code can query."

## 1. Existing structure survey

**Daemon boot site.** `runDaemonForeground` in `daemon.go:415-482`
is the CLI binary's init path: opens log, binds socket, constructs
`Daemon{}` (line 440), writes PID, resolves identity (line 456),
calls `d.startDaemonWS()` (line 468), installs signal handlers, runs
IPC accept loop.

`startDaemonWS` (line 589) is the only call site of
`ProbeAllHarnessVersions()` (line 670). It's gated on `wsURL != ""`,
`d.hostID != ""`, `d.hostSecret != ""` — a daemon without server
credentials skips that block entirely.

**Implication for plugin loading.** Plugin discovery is a local
concern (reads disk, talks to nothing). It must NOT be gated on WS
connectivity. The hook goes between `daemon: started` (line 453) and
the identity/config block (line 456) — earlier than
`ProbeAllHarnessVersions`'s WS-gated position.

**Existing in-memory registry parallel.** `harnessRegistry` in
`harness_iface.go:283` — package-level `map[string][]Harness{}`,
populated by `init()` in each `harness_<name>.go`, read-only after
init.

**Should plugin registry mirror this shape? No.** Three differences:
1. Discovery can fail per-entry (malformed YAML doesn't kill daemon);
   `init()`-based registration can't.
2. Set is mutable across daemon lifetime — `hearth plugin reload` is
   a follow-on; design must permit it.
3. Keys come from filesystem (directory basenames), not compile-time
   constants.

Plugin registry gets its own struct with `sync.RWMutex`, a single
`Load(dir)` entry point, and read methods that future `hearth plugin
reload` reuses unchanged. Owned by `Daemon{}` struct, not a package
global — keeps testing trivial.

## 2. YAML dependency

`go.mod` has no YAML dependency today (`grep yaml` over `*.go` and
`go.sum` returns nothing).

**Proposal: add `gopkg.in/yaml.v3`** as commit 1. Rationale:
- De facto Go YAML library.
- Repo already pulls heavier deps (`bubbletea`, `lipgloss`,
  `nhooyr.io/websocket`); YAML is uncontroversial.
- Manifest doc explicitly specifies YAML.
- Hand-rolling a parser is not worth the dep avoidance — manifests
  have nested maps, multiline strings, and unstructured
  `default_rules.when` blocks.

## 3. Proposed types

New files:
- `plugin_manifest.go` — type definitions + parsing + validation.
- `plugin_registry.go` — disk discovery + registry.

Split keeps each under ~200 lines; test files target one concern each.

Sketch (final field decisions during implementation):

```go
type PluginManifest struct {
    PluginType     string                `yaml:"plugin_type"`
    DisplayName    string                `yaml:"display_name"`
    Version        string                `yaml:"version"`
    ManifestSchema int                   `yaml:"manifest_schema"`
    Description    string                `yaml:"description"`
    Credentials    []PluginCredential    `yaml:"credentials"`
    Executable     string                `yaml:"executable"`
    Verbs          []PluginVerb          `yaml:"verbs"`
    DefaultRules   []PluginDefaultRule   `yaml:"default_rules"`

    // Derived at load time, not parsed from YAML:
    InstallID string `yaml:"-"` // directory basename
    SourceDir string `yaml:"-"` // absolute path to install dir
}

type PluginCredential struct {
    Name        string `yaml:"name"`
    Description string `yaml:"description"`
    Secret      bool   `yaml:"secret"`
}

type PluginVerb struct {
    Name        string                  `yaml:"name"`
    Description string                  `yaml:"description"`
    Args        map[string]PluginArgSpec `yaml:"args"`
    Output      string                  `yaml:"output"`
}

type PluginArgSpec struct {
    Type     string `yaml:"type"`
    Required bool   `yaml:"required"`
}

type PluginDefaultRule struct {
    Action   string         `yaml:"action"`
    Decision string         `yaml:"decision"`
    When     map[string]any `yaml:"when"` // unstructured; engine parses
}
```

JSON tags omitted — the manifest doesn't cross an IPC boundary in 1a.
Adding them later is a non-breaking change.

`supportedManifestSchemas = []int{1}` lives as a package-level
constant slice. `manifestSchemaSupported(int) bool` is the
membership check.

## 4. Discovery semantics

**Default path:** `~/.hearth/plugins/` (sibling of existing
`~/.hearth/daemon.log` and `daemon.pid`).

**Override:** env var `HEARTH_PLUGINS_DIR`. Single path only (not a
list) — tests need a tempdir; symlinks under the override dir handle
the dev-workflow story.

**Empty/missing directory:** log info ("no plugins directory at %s,
skipping plugin discovery"), continue with empty registry. Daemon is
fully functional without plugins.

**Malformed manifest:** log error with install id + parse error,
skip that one plugin, continue siblings. Never fatal.

**Validation gates (refuse, with logged reason):**
- `plugin_type` empty → refuse.
- `manifest_schema` absent OR not in `supportedManifestSchemas` →
  refuse. Default-zero after YAML unmarshal of missing field is the
  natural "absent" signal.
- `display_name` empty → refuse.
- `executable` empty → refuse (1a doesn't launch subprocesses, but
  storing a manifest with no executable is a known-bad state).
- `verbs` empty → **allow**. Degenerate but legitimate; soft warn,
  don't refuse.
- Duplicate `plugin_type` across install ids → **allow**.
  Multi-install case (`ha-home` + `ha-rental`, both `plugin_type:
  ha`) is exactly what the architecture supports.

**Derived fields:** set `InstallID = filepath.Base(dir)` and
`SourceDir = dir` on every manifest that survives validation.

## 5. Registry surface

```go
type PluginRegistry struct {
    mu          sync.RWMutex
    byInstallID map[string]PluginManifest
    order       []string // install ids, sorted; for ListPlugins determinism
}

func NewPluginRegistry() *PluginRegistry
func (r *PluginRegistry) Load(dir string) error
func (r *PluginRegistry) GetPluginByInstallID(id string) (PluginManifest, bool)
func (r *PluginRegistry) ListPlugins() []PluginManifest
```

`Load`:
- Scans + parses each manifest WITHOUT the lock.
- Takes the write lock for the final atomic swap (so a future
  reload's readers see the old set or the new, never half-built).
- Emits per-manifest error logs and one boot summary log.

`ListPlugins`: returns a slice copy (top-level), inner slices
shared (read-only after load).

No other methods for 1a. `GetPluginByPluginType` deferred — multi-
install means it'd return a list, and IAM integration isn't in 1a.

**Global vs injected:** owned by `Daemon{}` struct via a new
`plugins *PluginRegistry` field. Avoids package globals; tests
construct their own `Daemon{}`.

## 6. Boot hook

Exact site in `daemon.go` `runDaemonForeground`, immediately after
the `log.Printf("daemon: started ...")` at line 453, before the
identity/config block at line 456. About 6 lines:

```go
d.plugins = NewPluginRegistry()
pluginsDir := os.Getenv("HEARTH_PLUGINS_DIR")
if pluginsDir == "" {
    pluginsDir = filepath.Join(home, ".hearth", "plugins")
}
if err := d.plugins.Load(pluginsDir); err != nil {
    log.Printf("daemon: plugin load error: %v", err)
}
// summary log emitted by Load itself
```

Position rationale: before `startDaemonWS` (which is WS-gated), so a
daemon without server credentials still loads plugins. `home`
already in scope from line 417's `os.UserHomeDir()`.

Boot summary format (emitted by `Load`):
- `daemon: loaded 0 plugins from /Users/foo/.hearth/plugins
  (directory missing)`
- `daemon: loaded 3 plugins from /Users/foo/.hearth/plugins: gdrive,
  ha, slack`

Per-manifest errors log individually before the summary, prefixed
with the install id.

## 7. Tests

New files:
- `plugin_manifest_test.go` — parsing + validation (inline YAML
  strings).
- `plugin_registry_test.go` — disk discovery (testdata tree).

New testdata under `testdata/plugins/<scenario>/<plugin_id>/`.

Test matrix:

| Test | Fixture | Expected |
|---|---|---|
| `TestParseManifest_Valid` | inline HA-like YAML | round-trips to all expected fields |
| `TestValidateManifest_MissingPluginType` | inline | error mentions `plugin_type` |
| `TestValidateManifest_MissingSchema` | inline | error mentions schema |
| `TestValidateManifest_UnsupportedSchema` | `manifest_schema: 99` | error mentions unsupported version |
| `TestValidateManifest_EmptyDisplayName` | inline | error mentions display_name |
| `TestValidateManifest_EmptyExecutable` | inline | error mentions executable |
| `TestValidateManifest_ZeroVerbs` | inline | succeeds; soft warn |
| `TestRegistryLoad_Basic` | `basic/{ha,gdrive}/manifest.yaml` | both registered; sorted output |
| `TestRegistryLoad_SkipsMalformed` | `mixed/{good,bad}/manifest.yaml` | only good registered; error logged |
| `TestRegistryLoad_InstallIDDerivedFromDir` | `multiinstall/{ha-home,ha-rental}/`, both `plugin_type: ha` | both install ids registered |
| `TestRegistryLoad_EmptyDir` | empty tempdir | empty registry, no error |
| `TestRegistryLoad_MissingDir` | nonexistent path | empty registry, no error |
| `TestRegistryLoad_SkipsNonDirEntries` | tempdir with stray file | empty (or skips that one) |

Log capture: `bytes.Buffer` + `log.SetOutput` (matches
`harness_test.go` pattern).

Skip boot-hook integration tests for 1a — covered by unit tests; a
follow-on test comes with 1b IPC.

## 8. Commit plan

| # | Title | Files | Scope | After |
|---|---|---|---|---|
| 1 | `plugins: add yaml.v3 dependency` | `go.mod`, `go.sum` | S | builds clean, no behavior change |
| 2 | `plugins: PluginManifest types + YAML parsing` | new `plugin_manifest.go`; new `plugin_manifest_test.go` | M | YAML parses into typed structs; unit tests pass |
| 3 | `plugins: ValidateManifest + schema gates` | edits `plugin_manifest.go`; extends `plugin_manifest_test.go` | M | validation gates enforced and tested |
| 4 | `plugins: PluginRegistry with disk discovery` | new `plugin_registry.go`; new `plugin_registry_test.go`; new `testdata/plugins/*` | M | registry loads dir, skips malformed, Get/List, mutex in place |
| 5 | `plugins: wire registry into daemon boot` | edits `daemon.go` | S | daemon loads plugins at boot, summary log, `d.plugins` reachable. `HEARTH_PLUGINS_DIR` override included. |

4-5 commits. Each compiles cleanly, has passing tests,
independently reviewable. Commits 2-4 don't touch `daemon.go` so
they're trivially safe to land before the boot wiring.

## 9. Risks / open questions

1. **`default_rules.when` is unstructured** (`map[string]any`).
   Matches the doc's flexibility but typos become silent runtime
   issues when the rule-seeding sub-phase wires `default_rules` into
   the IAM rules table. Document with a TODO comment in
   `plugin_manifest.go`.

2. **`InstallID` uniqueness across reloads.** A user could `mv
   ~/.hearth/plugins/ha ~/.hearth/plugins/ha-old` and create a fresh
   `ha/`. Two install ids, same plugin_type — supported. Mobile UI /
   connection model keys on install id, not plugin_type. Flagged in
   adapters doc; re-flagging so 1b's IPC work doesn't accidentally
   key on plugin_type.

3. **Manifest schema absent treated as error.** Stricter than
   defaulting to 1; matches "daemon refuses unknown schema versions"
   from the doc. Rejects current third-party drafts that haven't
   seen the spec — correct, since there are none.

4. **Directory permission failures** (e.g. mode 000). `Load` logs
   and continues with empty registry, not panic. Test case is
   OS-flaky; cover with deliberate err-wrap + comment.

5. **Executable existence check at load time?** Deferred — would
   conflate broken-install with lazy-spawn errors. Land with the
   subprocess launcher in a later sub-phase.

6. **Icon file.** Manifest mentions `icon.png` as optional. 1a
   doesn't parse it; mobile UI concern, picked up with Resource
   Connections.

7. **Plugin type name format.** Doc uses lowercase-no-separators
   (`ha`, `gdrive`). Don't validate in 1a — third-party authors
   might use hyphens. When IAM action namespace
   (`external_resource.<plugin_type>.<verb>`) gets wired, regex-
   validate-at-load might be worth adding. Settle the allowed
   character set before publishing the substrate.

8. **Reload concurrency.** Registry is structured for `hearth plugin
   reload` (RWMutex, atomic swap in `Load`). 1a doesn't expose a
   reload path. Risk: 1b's reload IPC handler forgets to test that
   in-flight readers don't see partial state. Defense is the atomic
   swap; documented on `Load`'s comment.

## Cross-references

- `hearth-cmd/docs/external-resource-adapters.md` — authoritative
  architecture, manifest schema.
- `hearth-cmd/docs/iam-checkrules-generalization-plan.md` — phase 0,
  shipped 2026-05-14.
- Memory: `project_external_resource_adapters.md`,
  `project_iam_planning.md`.
