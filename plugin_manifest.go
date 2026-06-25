package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// supportedManifestSchemas lists the manifest_schema integer values
// this binary knows how to parse. Plugins declaring an unsupported
// version are refused at load time with a clear error — better than
// silently parsing an incompatible shape. Bump (and add entry above)
// when the manifest format changes in a backwards-incompatible way.
//
// Schema 1: subprocess-only. Manifest requires executable; verbs have
//           no http: block; daemon launches a subprocess per active
//           resource_connection and speaks JSON-RPC.
// Schema 2: introduces declarative adapters. Verbs may carry an http:
//           block describing the upstream call; manifest may omit
//           executable in that case; daemon runs the call in-process.
//           Mixed manifests (some verbs http, some not) are refused —
//           classification is per-install, not per-verb. See
//           hearth-cmd/docs/ha-yaml-adapter-plan.md.
var supportedManifestSchemas = []int{1, 2}

func manifestSchemaSupported(v int) bool {
	for _, s := range supportedManifestSchemas {
		if s == v {
			return true
		}
	}
	return false
}

// PluginManifest is the parsed shape of a plugin install's
// manifest.yaml. Mirrors the schema in
// hearth-cmd/docs/external-resource-adapters.md §"Manifest schema
// (YAML)". SourceDir is derived at load time from the install
// directory, not parsed from YAML.
type PluginManifest struct {
	// PluginSlug is the stable identifier for this plugin
	// ("verge_labs/ha", "acme/ha"). Used in the IAM action namespace
	// `external_resource.<plugin_slug>.<verb>` and as the install
	// directory path relative to the plugins root: the registry refuses
	// any install whose directory path doesn't equal manifest.plugin_slug.
	// First-party plugins use a namespace prefix ("verge_labs/ha");
	// community plugins follow the same convention to avoid collisions.
	//
	// Multi-instance lives at the resource_connections layer (one
	// install backs many connections), NOT here. There is exactly
	// one install per plugin_slug per host.
	PluginSlug string `yaml:"plugin_slug"`

	// Namespace is the publisher scope for this plugin (e.g. "verge_labs",
	// "acme"). Optional but recommended. When set, ValidateManifest checks
	// that plugin_slug starts with namespace + "/". The install directory
	// must mirror this layout:
	//   ~/.hearth/plugins/verge_labs/ha/manifest.yaml  → namespace=verge_labs, slug=verge_labs/ha
	Namespace string `yaml:"namespace,omitempty"`

	// Author is the human-readable publisher name (e.g. "Verge Labs").
	// Informational only; shown in `hearth plugin list`.
	Author string `yaml:"author,omitempty"`

	// MinDaemonVersion is the minimum hearth binary version required to
	// load and execute this plugin. Semver string (e.g. "1.3.0"). Absent
	// or empty means no requirement. The daemon refuses to load the plugin
	// if its own version is lower, emitting a clear error rather than
	// silently mishandling unsupported manifest features.
	MinDaemonVersion string `yaml:"min_daemon_version,omitempty"`

	DisplayName    string `yaml:"display_name"`
	Version        string `yaml:"version"`
	ManifestSchema int    `yaml:"manifest_schema"`
	Description    string `yaml:"description"`

	// AuthScheme declares how this plugin authenticates to the external
	// service. Informational — shown as a badge in the install wizard
	// and plugin browser so operators know what credential setup is
	// required before installing.
	AuthScheme string `yaml:"auth_scheme,omitempty"` // see AuthScheme* constants

	Credentials  []PluginCredential  `yaml:"credentials"`
	Executable   string              `yaml:"executable"`
	Verbs        []PluginVerb        `yaml:"verbs"`
	DefaultRules []PluginDefaultRule `yaml:"default_rules"`

	// ConfigSchema is the manifest's JSON Schema for the per-
	// resource_connection `config` blob (non-sensitive params: DB host,
	// OAuth client_id, etc.). Reported to the server verbatim and
	// stored on plugin_installs.config_schema. The webview's new-
	// connection form renders fields from this schema and the server
	// validates resource_connections.config against it at write time.
	// Free-form so authors can use any JSON Schema features the server
	// supports (today: top-level type/properties/required; full
	// draft-07 validation via santhosh-tekuri/jsonschema). Empty =
	// config is unconstrained. Plan §11 step 9.
	ConfigSchema map[string]interface{} `yaml:"config_schema"`

	// Snapshot, when non-nil, declares how the daemon enumerates this
	// adapter's entities for IAM evaluation. Optional — declarative
	// adapters without a snapshot still serve verbs (rules just can't
	// predicate on entity kind/labels). Schema v2 only.
	Snapshot *SnapshotSpec `yaml:"snapshot,omitempty"`

	// SourceDir is the absolute path to the install directory. Set
	// by the registry at load time. Used later by subprocess-launch
	// code to resolve Executable relative to it without re-deriving.
	SourceDir string `yaml:"-"`

	// Source classifies how this manifest executes:
	//   'binary'      — Executable points at a subprocess the daemon
	//                   launches per active resource_connection.
	//   'declarative' — manifest declares each verb's http: block
	//                   and the daemon runs the call in-process. Not
	//                   yet supported; the classifier returns 'binary'
	//                   for every manifest today (see
	//                   ClassifyManifestSource).
	// Computed at load time, not parsed from YAML. Reported to the
	// server so plugin_installs.source stays in sync. See
	// hearth-cmd/docs/ha-yaml-adapter-plan.md.
	Source string `yaml:"-"`
}

// SourceBinary marks subprocess plugins; SourceDeclarative marks
// in-daemon HTTP adapters. Mirrors plugin_installs.source values.
const (
	SourceBinary      = "binary"
	SourceDeclarative = "declarative"
)

// AuthScheme values for PluginManifest.AuthScheme.
const (
	AuthSchemeServiceAccount = "service_account" // Google-style service account JSON key + domain-wide delegation
	AuthSchemeOAuth2User     = "oauth2_user"      // per-user OAuth2 flow; credentials stored as connection_identity
	AuthSchemeAPIKey         = "api_key"          // static API key per identity
	AuthSchemeNone           = "none"             // no credentials required
)

// ClassifyManifestSource decides whether a parsed manifest describes a
// binary plugin or an in-daemon declarative adapter. The rule is
// simple: any verb with an http: block makes the manifest declarative;
// otherwise binary. Validation (ValidateManifest) enforces the
// consistency rules — mixed manifests (some verbs http, some not) are
// refused; schema must be >= 2 for declarative; executable must be
// empty for declarative and non-empty for binary.
func ClassifyManifestSource(m PluginManifest) string {
	for _, v := range m.Verbs {
		if v.HTTP != nil {
			return SourceDeclarative
		}
	}
	return SourceBinary
}

// PluginCredential names a credential the plugin needs (URL, token,
// API key, etc.). The daemon's credential broker resolves these by
// name against the secrets vault, scoped per Resource Connection, and
// hands plaintext to the plugin subprocess at Init. Plugin authors
// never deal in ciphertext or the vault directly.
type PluginCredential struct {
	Name        string `yaml:"name"        json:"name"`
	Description string `yaml:"description" json:"description,omitempty"`
	// Secret distinguishes plain config (URLs, flags) from material
	// the vault should encrypt. Both ride through `credentials` for a
	// single source of truth on plugin inputs.
	Secret bool `yaml:"secret" json:"secret"`
	// Type, when set, tells the declarative executor that this
	// credential requires special handling before it is made available
	// in templates. Currently recognised values:
	//
	//   "service_account_json" — the secret value is a Google Cloud
	//       service account JSON key file. The executor generates a
	//       signed JWT and exchanges it for a short-lived access_token
	//       via the Google OAuth2 token endpoint. Templates reference
	//       the result as {{credentials.<name>.access_token}}. Token
	//       caching across verb calls is handled automatically.
	//
	// Leaving Type empty (the default) keeps the flat string behaviour:
	// {{credentials.<name>}} → the raw secret value.
	Type string `yaml:"type,omitempty" json:"type,omitempty"`
	// Scopes lists the OAuth2 scope URLs to request when Type is
	// "service_account_json". Required when Type is set; ignored
	// otherwise.
	Scopes []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
	// ImpersonationConfigRef, when Type is "service_account_json",
	// names the config_schema key whose value is the Workspace user
	// email the service account should impersonate (domain-wide
	// delegation). If empty, no impersonation is requested (the token
	// is issued for the service account itself).
	ImpersonationConfigRef string `yaml:"impersonation_config_ref,omitempty" json:"impersonation_config_ref,omitempty"`
	// WizardGuide is shown in the NewConnectionWizard when prompting
	// the operator to paste this credential. Rendered as plain text.
	// Optional.
	WizardGuide string `yaml:"wizard_guide,omitempty" json:"wizard_guide,omitempty"`
	// WizardLink + WizardLinkLabel render as a tappable link below
	// the wizard_guide text, deep-linking to the provider's
	// token-creation page. Optional.
	WizardLink      string `yaml:"wizard_link,omitempty"       json:"wizard_link,omitempty"`
	WizardLinkLabel string `yaml:"wizard_link_label,omitempty" json:"wizard_link_label,omitempty"`
}

// PluginVerb declares one tool the plugin exposes to the agent.
// Translates to an IAM action `external_resource.<plugin_slug>.<name>`.
// Args is unstructured-typed for now (string/int/bool); future schema
// versions may tighten this.
type PluginVerb struct {
	Name        string                   `yaml:"name"`
	Description string                   `yaml:"description"`
	Args        map[string]PluginArgSpec `yaml:"args"`
	// Output declares the wire shape of stdout for this verb
	// (`json`/`text`). Convention today, not enforced by the
	// daemon — the agent's prompt-side description uses it so the
	// model knows what to expect.
	Output string `yaml:"output"`
	// HTTP, when non-nil, declares this verb as a declarative HTTP
	// call the daemon runs in-process. Requires manifest_schema >= 2.
	// Presence of any verb with HTTP forces the manifest's
	// classification to 'declarative' (see ClassifyManifestSource).
	HTTP *VerbHTTPSpec `yaml:"http,omitempty"`

	// GrantCategories lists the classification labels used to group this
	// verb in the mobile grants editor. Values are free-form strings
	// chosen by the plugin author (e.g. ["read"], ["write", "locks"]).
	// The UI groups verbs sharing a category under a common header,
	// making it easier to review and grant permissions for a large
	// plugin. Optional; absent verbs appear ungrouped.
	GrantCategories []string `yaml:"grant_categories,omitempty" json:"grant_categories,omitempty"`

	// Deprecated marks this verb as superseded. The daemon continues to
	// serve it; the mobile UI surfaces a deprecation hint. Use alongside
	// ReplacedBy to guide operators toward the replacement.
	Deprecated bool `yaml:"deprecated,omitempty" json:"deprecated,omitempty"`
	// ReplacedBy names the verb that supersedes this one. Informational;
	// the daemon does not auto-redirect calls. Only meaningful when
	// Deprecated is true.
	ReplacedBy string `yaml:"replaced_by,omitempty" json:"replaced_by,omitempty"`
}

// VerbHTTPSpec describes a single HTTP call backing a declarative verb.
// Templates (URL, headers, body) are mustache-style and substituted
// against {config, credentials, args} at invoke time by the
// declarative executor (see resource_decl.go). Schema v2.
type VerbHTTPSpec struct {
	Method   string            `yaml:"method"`            // GET / POST / PUT / PATCH / DELETE
	URL      string            `yaml:"url"`               // mustache-templated
	Headers  map[string]string `yaml:"headers,omitempty"` // mustache-templated values
	Body     string            `yaml:"body,omitempty"`    // mustache-templated; empty for verbs with no body
	Response VerbHTTPResponse  `yaml:"response,omitempty"`
}

// VerbHTTPResponse declares how the executor interprets the upstream
// response. SuccessStatus defaults to [200] when empty. Output is
// either a JSONPath ($.field.subfield) extracted from the response
// body, a literal string to return verbatim, or empty (return raw
// body).
type VerbHTTPResponse struct {
	SuccessStatus []int  `yaml:"success_status,omitempty"`
	Output        string `yaml:"output,omitempty"`
}

// SnapshotSpec describes how the daemon enumerates the connection's
// entities for IAM evaluation. Optional at the manifest level — a
// declarative adapter without a snapshot block can still serve verbs
// (rules just can't predicate on entity kind/labels for it). Schema
// v2.
//
// HTTP follows the same templating rules as verb HTTP blocks, but the
// template scope has no `args` (snapshots aren't per-call). Extract
// runs against the response body and emits one Entity per item the
// iterate path matches.
type SnapshotSpec struct {
	Description string         `yaml:"description,omitempty"`
	HTTP        VerbHTTPSpec   `yaml:"http"`
	Extract     ExtractSpec    `yaml:"extract"`
}

// ExtractSpec maps an upstream response into a list of normalized
// entities. All path fields are the tiny JSONPath subset shared with
// the verb extractor: $ + dotted descent + optional trailing [*] for
// iterate.
//
// Per-item fields (EntityID, Kind.JSONPath, label values, Parent) are
// JSONPaths rooted at each iterated item, not the whole response.
type ExtractSpec struct {
	// Iterate selects the array of items in the response body. Common
	// shapes: "$[*]" when the response IS an array; "$.results[*]"
	// when it's wrapped.
	Iterate string `yaml:"iterate"`
	// EntityID is a JSONPath into each item naming the stable id the
	// daemon stores. Required.
	EntityID string `yaml:"entity_id"`
	// Kind derives the entity's kind string. Exactly one of the three
	// sub-fields must be set; see ExtractKindSpec.
	Kind ExtractKindSpec `yaml:"kind"`
	// Labels maps label-name → JSONPath-into-item. Missing values are
	// silently omitted from the resulting Entity's Labels map (labels
	// are metadata; absence isn't an error).
	Labels map[string]string `yaml:"labels,omitempty"`
	// Parent is an optional JSONPath. Empty or absent → no parent.
	Parent string `yaml:"parent,omitempty"`
}

// ExtractKindSpec is a small union for "how to derive the kind
// string." Exactly one of the fields is set per manifest.
//
//   - Literal: fixed value used for every entity. e.g. "slack.channel".
//   - JSONPath: JSONPath into the current item; the leaf becomes kind.
//     e.g. "$.kind" when the upstream already names it.
//   - PrefixWithEntityDomain: literal prefix concatenated with the
//     portion of entity_id before its first ".". e.g. "ha." applied
//     to entity_id "light.kitchen" yields "ha.light". The HA shape.
type ExtractKindSpec struct {
	Literal                string `yaml:"literal,omitempty"`
	JSONPath               string `yaml:"jsonpath,omitempty"`
	PrefixWithEntityDomain string `yaml:"prefix_with_entity_domain,omitempty"`
}

type PluginArgSpec struct {
	Type     string `yaml:"type"`
	Required bool   `yaml:"required"`
}

// PluginDefaultRule is a rule-row seed used by the connection-create
// path: when a new Resource Connection of this plugin_slug is
// created, the daemon copies these into the IAM rules table for that
// connection's resource id. Users edit them post-hoc in the mobile
// UI. The `When` block is unstructured here because the predicate
// grammar is per-resource-kind (see hearth-cmd/docs/iam-planning.md
// §"Predicate language") — wiring `default_rules` into the rules
// table will validate `When` against that grammar.
//
// TODO(resource-plugins): validate `When` against the engine's
// predicate grammar at rule-seed time (the seed-into-rules-table
// sub-phase will land this).
type PluginDefaultRule struct {
	Action   string         `yaml:"action"   json:"action"`
	Decision string         `yaml:"decision" json:"decision"`
	When     map[string]any `yaml:"when"     json:"when,omitempty"`
}

// ParseManifest unmarshals a manifest.yaml byte slice into a
// PluginManifest. Pure: doesn't read disk, doesn't validate semantics.
// Use ReadManifestFile for the disk path, and ValidateManifest for the
// semantic checks (refusing missing fields, unsupported schema, etc.)
// landing in a follow-on commit.
func ParseManifest(data []byte) (PluginManifest, error) {
	var m PluginManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return PluginManifest{}, fmt.Errorf("manifest yaml parse: %w", err)
	}
	return m, nil
}

// ValidateManifest enforces semantic requirements that ParseManifest
// can't catch — the gates that decide whether a parsed manifest is
// safe to participate in later substrate steps (subprocess launch,
// IAM action namespace emission, prompt-side surfacing). Returns nil
// when the manifest is admissible.
//
// Strict gates (refuse):
//   - PluginSlug non-empty.
//   - ManifestSchema present (non-zero) AND in
//     supportedManifestSchemas. A zero value means the field was
//     missing from the YAML — required, not defaulted.
//   - DisplayName non-empty — operators need something readable in
//     logs / mobile UI.
//   - For binary classification: Executable non-empty; no verbs
//     declare http: blocks.
//   - For declarative classification: ManifestSchema >= 2;
//     Executable empty; every verb declares an http: block (mixed
//     manifests refused per docs/ha-yaml-adapter-plan.md); each
//     http: block has non-empty Method + URL.
//
// Permissive (intentionally not refused):
//   - Verbs may be empty in a binary manifest. A zero-verb plugin is
//     degenerate but legitimate for tests and early development.
//     Declarative manifests with zero verbs are also permitted — the
//     "every verb has http" check is vacuously true.
func ValidateManifest(m PluginManifest) error {
	if m.PluginSlug == "" {
		return fmt.Errorf("manifest: plugin_slug is required")
	}
	if m.Namespace != "" && !strings.HasPrefix(m.PluginSlug, m.Namespace+"/") {
		return fmt.Errorf("manifest: plugin_slug %q must start with namespace %q followed by '/'", m.PluginSlug, m.Namespace)
	}
	if m.ManifestSchema == 0 {
		return fmt.Errorf("manifest: manifest_schema is required (zero or missing)")
	}
	if !manifestSchemaSupported(m.ManifestSchema) {
		return fmt.Errorf("manifest: manifest_schema=%d not supported by this binary (supported: %v)",
			m.ManifestSchema, supportedManifestSchemas)
	}
	if m.DisplayName == "" {
		return fmt.Errorf("manifest: display_name is required")
	}

	src := ClassifyManifestSource(m)
	switch src {
	case SourceBinary:
		if m.Executable == "" {
			return fmt.Errorf("manifest: executable is required for binary plugins")
		}
	case SourceDeclarative:
		if m.ManifestSchema < 2 {
			return fmt.Errorf("manifest: declarative verbs (http: blocks) require manifest_schema >= 2; got %d", m.ManifestSchema)
		}
		if m.Executable != "" {
			return fmt.Errorf("manifest: declarative plugins must not declare executable; remove the field or move it to a separate binary plugin")
		}
		for _, v := range m.Verbs {
			if v.HTTP == nil {
				return fmt.Errorf("manifest: mixed verbs not allowed — verb %q has no http: block in a declarative manifest", v.Name)
			}
			if v.HTTP.Method == "" {
				return fmt.Errorf("manifest: verb %q http.method is required", v.Name)
			}
			if v.HTTP.URL == "" {
				return fmt.Errorf("manifest: verb %q http.url is required", v.Name)
			}
		}
		if m.Snapshot != nil {
			if err := validateSnapshot(*m.Snapshot); err != nil {
				return fmt.Errorf("manifest: snapshot: %w", err)
			}
		}
	}
	return nil
}

// validateSnapshot enforces the shape constraints on a manifest's
// snapshot block: HTTP must have method + url; extract must declare
// iterate + entity_id; kind must set exactly one of the three
// derivation modes. Per-field JSONPath syntax isn't validated here —
// the executor surfaces parse errors at first run with a useful
// upstream-response context.
func validateSnapshot(s SnapshotSpec) error {
	if s.HTTP.Method == "" {
		return fmt.Errorf("http.method is required")
	}
	if s.HTTP.URL == "" {
		return fmt.Errorf("http.url is required")
	}
	if s.Extract.Iterate == "" {
		return fmt.Errorf("extract.iterate is required")
	}
	if s.Extract.EntityID == "" {
		return fmt.Errorf("extract.entity_id is required")
	}
	n := 0
	if s.Extract.Kind.Literal != "" {
		n++
	}
	if s.Extract.Kind.JSONPath != "" {
		n++
	}
	if s.Extract.Kind.PrefixWithEntityDomain != "" {
		n++
	}
	if n != 1 {
		return fmt.Errorf("extract.kind must set exactly one of literal, jsonpath, prefix_with_entity_domain")
	}
	return nil
}

// ReadManifestFile reads a manifest.yaml off disk and parses it.
// Returns a wrapped error including the path on either read or parse
// failure so the caller's log line is operator-friendly.
func ReadManifestFile(path string) (PluginManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PluginManifest{}, fmt.Errorf("read manifest %s: %w", path, err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		return PluginManifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}
