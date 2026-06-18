//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// daemonStatus prints a one-shot snapshot of who the user is, this host,
// and the fleet (other hosts + agent instances). Layout mirrors the
// mobile About sheet plus the live agents list.
//
// Degraded mode: when the daemon isn't running, sections 1-3 still print
// from local config so a logged-out / stopped-host user gets something
// useful instead of just "host is not running".
func daemonStatus() {
	if !isDaemonRunning() {
		printOfflineStatus()
		return
	}

	ident, err := requestIdentity()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth: %v\n", err)
		return
	}
	local, _ := requestStatus()

	// Fleet lists are best-effort; render whatever comes back. An offline
	// WS or server hiccup shouldn't suppress the local sections above it.
	var hosts []hostsListEntry
	var instances []instancesListEntry
	if ident.WSConnected {
		hosts = fetchHosts()
		instances = fetchAgentInstances(currentOrgID(ident.Organizations))
	}

	out := os.Stdout
	printIdentitySection(out, ident)
	printSubscriptionSection(out, ident)
	printVersionSection(out, ident)
	printThisHostSection(out, ident, local)
	printHarnessesSection(out, ident)
	printPluginsSection(out)
	printFleetHostsSection(out, ident, hosts)
	printFleetAgentsSection(out, instances, hosts)
}

// printOfflineStatus is the degraded path used when the daemon is
// stopped. Sections that need server data (fleet, WS state) are absent.
func printOfflineStatus() {
	email := readConfigValue("email")
	out := os.Stdout
	fmt.Fprintln(out, "ACCOUNT")
	if email == "" {
		fmt.Fprintln(out, "  (not logged in — run `hearth login`)")
	} else {
		fmt.Fprintf(out, "  email: %s\n", email)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "VERSION")
	fmt.Fprintf(out, "  %s\n", strings.TrimPrefix(versionString(), "hearth "))
	if wsURL != "" {
		fmt.Fprintf(out, "  server: %s\n", wsURL)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "HOST")
	fmt.Fprintln(out, "  host is not running — run `hearth start`")
}

func printIdentitySection(out *os.File, ident *ipcResponse) {
	fmt.Fprintln(out, "ACCOUNT")
	if ident.Email == "" {
		fmt.Fprintln(out, "  (no account info yet)")
	} else {
		fmt.Fprintf(out, "  email: %s\n", ident.Email)
	}
	if cur := currentOrg(ident.Organizations); cur != nil {
		fmt.Fprintf(out, "  household: %s (%s)\n", cur.Name, cur.Slug)
		fmt.Fprintf(out, "  role: %s\n", cur.Role)
	}
	fmt.Fprintln(out)
}

func printSubscriptionSection(out *os.File, ident *ipcResponse) {
	fmt.Fprintln(out, "SUBSCRIPTION")
	cur := currentOrg(ident.Organizations)
	if cur == nil {
		fmt.Fprintln(out, "  (no current household)")
		fmt.Fprintln(out)
		return
	}
	if !cur.IsPro {
		fmt.Fprintln(out, "  plan: Free")
		fmt.Fprintln(out)
		return
	}
	source := cur.ProSource
	switch source {
	case "subscription":
		source = "Subscription"
	case "offer":
		source = "Offer code"
	case "":
		source = "(unknown)"
	}
	fmt.Fprintln(out, "  plan: Pro")
	fmt.Fprintf(out, "  source: %s\n", source)
	fmt.Fprintln(out)
}

func printVersionSection(out *os.File, ident *ipcResponse) {
	fmt.Fprintln(out, "VERSION")
	fmt.Fprintf(out, "  %s\n", strings.TrimPrefix(versionString(), "hearth "))
	if ident.ServerURL != "" {
		fmt.Fprintf(out, "  server: %s\n", ident.ServerURL)
	}
	fmt.Fprintln(out)
}

func printThisHostSection(out *os.File, ident *ipcResponse, instances []instanceInfo) {
	pidData, _ := os.ReadFile(daemonPidPath())
	pid := strings.TrimSpace(string(pidData))

	fmt.Fprintln(out, "THIS HOST")
	if ident.Hostname != "" {
		fmt.Fprintf(out, "  hostname: %s\n", ident.Hostname)
	}
	if ident.HostID != "" {
		fmt.Fprintf(out, "  host_id: %s\n", shortID(ident.HostID))
	}
	if pid != "" {
		fmt.Fprintf(out, "  pid: %s\n", pid)
	}
	if ident.StartedAt != "" {
		if t, err := time.Parse(time.RFC3339, ident.StartedAt); err == nil {
			fmt.Fprintf(out, "  uptime: %s\n", humanDuration(time.Since(t)))
		}
	}
	connState := "offline"
	if ident.WSConnected {
		connState = "connected"
	}
	fmt.Fprintf(out, "  server connection: %s\n", connState)
	if ident.AgentHomePath != "" {
		fmt.Fprintf(out, "  agent home: %s\n", ident.AgentHomePath)
	}

	if len(instances) == 0 {
		fmt.Fprintln(out, "  active instances: (none)")
		fmt.Fprintln(out)
		return
	}
	fmt.Fprintf(out, "  active instances: %d\n", len(instances))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "    ID\tAGENT\tPROJECT\tCWD")
	for _, s := range instances {
		fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\n", shortID(s.AIAgentInstanceID), s.Agent, s.Project, s.Cwd)
	}
	tw.Flush()
	fmt.Fprintln(out)
}

// printHarnessesSection shows which harness CLIs the daemon can resolve on
// its own PATH and which the server has recorded for this host. Mismatch
// between the two is the signal: empty "server" with non-empty "local"
// means the daemon's report_host_harnesses never landed (e.g. WS wasn't up
// at startup, or the host's org check rejected); that's also why the
// agent-create wizard would show every harness as unavailable.
func printHarnessesSection(out *os.File, ident *ipcResponse) {
	fmt.Fprintln(out, "HARNESSES")

	local, versions := requestProbedHarnesses()

	var server []string
	var serverNote string
	switch {
	case !ident.WSConnected || ident.HostID == "":
		serverNote = "(offline — can't query server)"
	case currentOrgID(ident.Organizations) == "":
		serverNote = "(no current household — can't query server)"
	default:
		s, err := fetchServerHarnessesForHost(ident.HostID, currentOrgID(ident.Organizations))
		if err != nil {
			serverNote = fmt.Sprintf("(server error: %v)", err)
		} else {
			server = s
		}
	}

	// Union of names from both sides, sorted, so inconsistencies sit on
	// adjacent columns of the same row.
	seen := map[string]bool{}
	for _, n := range local {
		seen[n] = true
	}
	for _, n := range server {
		seen[n] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)

	localSet := map[string]bool{}
	for _, n := range local {
		localSet[n] = true
	}
	serverSet := map[string]bool{}
	for _, n := range server {
		serverSet[n] = true
	}

	if len(names) == 0 && serverNote == "" {
		fmt.Fprintln(out, "  (none on PATH, none recorded on server)")
		fmt.Fprintln(out)
		return
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  HARNESS\tON PATH\tON SERVER\tVERSION\t")
	mark := func(b bool) string {
		if b {
			return "yes"
		}
		return "-"
	}
	for _, n := range names {
		serverCell := mark(serverSet[n])
		if serverNote != "" {
			serverCell = "?"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t\n", n, mark(localSet[n]), serverCell, formatHarnessVersion(versions[n]))
	}
	tw.Flush()
	if serverNote != "" {
		fmt.Fprintf(out, "  %s\n", serverNote)
	}
	fmt.Fprintln(out)
}

// formatHarnessVersion renders the per-harness version cell for
// `hh status`. Empty → "?" so an absent entry is visually distinct
// from "0.0.0" or similar real values. A detected version gets a
// suffix: " (tested)" if in KnownTestedVersions, " (untested)" if
// outside but above MinimumVersion, " (below min: X)" if below the
// declared floor.
func formatHarnessVersion(v ipcHarnessVersion) string {
	if v.Installed == "" && v.Minimum == "" && len(v.Tested) == 0 {
		return "?"
	}
	if v.Installed == "" {
		return "(probe failed)"
	}
	if v.Minimum != "" && !semverGTE(v.Installed, v.Minimum) {
		return v.Installed + " (below min: " + v.Minimum + ")"
	}
	for _, t := range v.Tested {
		if t == v.Installed {
			return v.Installed + " (tested)"
		}
	}
	if len(v.Tested) == 0 {
		return v.Installed
	}
	return v.Installed + " (untested)"
}

// requestProbedHarnesses asks the running daemon for the harness names
// whose binary it can resolve on PATH, plus per-harness version metadata
// (detected version, minimum supported, tested set). Returns (nil, nil)
// on any IPC error so the caller renders "(none)" rather than failing
// the whole status.
func requestProbedHarnesses() ([]string, map[string]ipcHarnessVersion) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 2*time.Second)
	if err != nil {
		return nil, nil
	}
	defer conn.Close()
	msg, _ := json.Marshal(ipcRequest{Type: "harnesses"})
	msg = append(msg, '\n')
	if _, err := conn.Write(msg); err != nil {
		return nil, nil
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, nil
	}
	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, nil
	}
	return resp.Harnesses, resp.HarnessVersions
}

// fetchServerHarnessesForHost calls list_harnesses over the daemon WS and
// returns the names (not ids) the server has on file as available for this
// host. Empty list ≠ error — server returns [] when it knows about the
// host but has nothing recorded, or when the org-binding check rejects.
func fetchServerHarnessesForHost(hostID, orgID string) ([]string, error) {
	data, err := sendWSRequest("list_harnesses", map[string]interface{}{
		"host_id":         hostID,
		"organization_id": orgID,
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Harnesses []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"harnesses"`
		AvailableIDs []string `json:"available_harness_ids"`
		Error        string   `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	nameByID := map[string]string{}
	for _, h := range resp.Harnesses {
		nameByID[h.ID] = h.Name
	}
	out := []string{}
	for _, id := range resp.AvailableIDs {
		if n, ok := nameByID[id]; ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func printPluginsSection(out *os.File) {
	plugins, pluginsDir := requestPluginList()
	dirLabel := ""
	if pluginsDir != "" {
		dirLabel = "  (" + pluginsDir + ")"
	}
	fmt.Fprintf(out, "PLUGINS%s\n", dirLabel)
	if len(plugins) == 0 {
		fmt.Fprintln(out, "  (none installed)")
		fmt.Fprintln(out)
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  SLUG\tAUTHOR\tVERSION\tVERBS\t")
	for _, p := range plugins {
		author := p.Author
		if author == "" {
			author = "-"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t\n", p.PluginSlug, author, p.Version, p.Verbs)
	}
	tw.Flush()
	fmt.Fprintln(out)
}

func requestPluginList() (plugins []struct {
	PluginSlug string `json:"plugin_slug"`
	Author     string `json:"author,omitempty"`
	Version    string `json:"version"`
	Verbs      int    `json:"verbs"`
}, pluginsDir string) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 2*time.Second)
	if err != nil {
		return nil, ""
	}
	defer conn.Close()
	msg, _ := json.Marshal(ipcRequest{Type: "plugin_list"})
	msg = append(msg, '\n')
	if _, err := conn.Write(msg); err != nil {
		return nil, ""
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, ""
	}
	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, ""
	}
	var inner struct {
		Plugins []struct {
			PluginSlug string `json:"plugin_slug"`
			Author     string `json:"author,omitempty"`
			Version    string `json:"version"`
			Verbs      int    `json:"verbs"`
		} `json:"plugins"`
		PluginsDir string `json:"plugins_dir"`
	}
	if err := json.Unmarshal(resp.Data, &inner); err != nil {
		return nil, ""
	}
	return inner.Plugins, inner.PluginsDir
}

func printFleetHostsSection(out *os.File, ident *ipcResponse, hosts []hostsListEntry) {
	fmt.Fprintln(out, "HOSTS")
	if len(hosts) == 0 {
		fmt.Fprintln(out, "  (none — server unreachable or none enrolled)")
		fmt.Fprintln(out)
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  HOSTNAME\tHOST_ID\tDESIRED\tLAST SEEN\t")
	for _, h := range hosts {
		marker := "  "
		if h.HostID == ident.HostID {
			marker = "* "
		}
		name := h.Hostname
		if name == "" {
			name = "(unnamed)"
		}
		desired := h.DesiredStatus
		if desired == "" {
			desired = "?"
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%s\t%s\t\n", marker, name, shortID(h.HostID), desired, humanLastSeen(h.LastSeenAt))
	}
	tw.Flush()
	fmt.Fprintln(out)
}

func printFleetAgentsSection(out *os.File, instances []instancesListEntry, hosts []hostsListEntry) {
	fmt.Fprintln(out, "AGENTS")
	if len(instances) == 0 {
		fmt.Fprintln(out, "  (none in current household)")
		fmt.Fprintln(out)
		return
	}
	hostNames := map[string]string{}
	for _, h := range hosts {
		hostNames[h.HostID] = h.Hostname
	}
	sort.SliceStable(instances, func(i, j int) bool {
		return instances[i].LastActivityAt > instances[j].LastActivityAt
	})

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  ID\tNAME\tHOST\tSTATUS\tPID\tLAST ACTIVITY\tEPHEMERAL\t")
	for _, a := range instances {
		host := hostNames[a.HostID]
		if host == "" {
			host = shortID(a.HostID)
		}
		eph := ""
		if a.IsTemp {
			eph = "yes"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t\n",
			shortID(a.ID), a.Name, host, a.Status, a.PIDStatus,
			humanLastSeen(a.LastActivityAt), eph)
	}
	tw.Flush()
	fmt.Fprintln(out)
}

// =============================================================================
// IPC + WS helpers
// =============================================================================

// requestIdentity sends the "identity" IPC and returns the parsed response.
func requestIdentity() (*ipcResponse, error) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to host: %v", err)
	}
	defer conn.Close()

	msg, _ := json.Marshal(ipcRequest{Type: "identity"})
	msg = append(msg, '\n')
	if _, err := conn.Write(msg); err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read identity: %v", err)
	}
	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("parse identity: %v", err)
	}
	if resp.Type == "error" {
		return nil, fmt.Errorf("%s", resp.Message)
	}
	return &resp, nil
}

// requestStatus sends the "status" IPC and returns local active instances.
func requestStatus() ([]instanceInfo, error) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	msg, _ := json.Marshal(ipcRequest{Type: "status"})
	msg = append(msg, '\n')
	conn.Write(msg)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	return resp.Instances, nil
}

type hostsListEntry struct {
	HostID        string `json:"host_id"`
	Hostname      string `json:"hostname"`
	DesiredStatus string `json:"desired_status"`
	LastSeenAt    string `json:"last_seen_at"`
	AgentHomePath  string `json:"agent_home_path"`
}

func fetchHosts() []hostsListEntry {
	data, err := sendWSRequest("list_hosts", nil)
	if err != nil {
		return nil
	}
	var resp struct {
		Hosts []hostsListEntry `json:"hosts"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Hosts
}

type instancesListEntry struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	PIDStatus      string `json:"pid_status"`
	HostID         string `json:"host_id"`
	RetiredAt      string `json:"retired_at"`
	LastActivityAt string `json:"last_activity_at"`
	IsTemp         bool   `json:"is_temp"`
}

func fetchAgentInstances(orgID string) []instancesListEntry {
	if orgID == "" {
		return nil
	}
	data, err := sendWSRequest("list_ai_agent_instances", map[string]interface{}{
		"organization_id": orgID,
	})
	if err != nil {
		return nil
	}
	var resp struct {
		Instances []instancesListEntry `json:"ai_agent_instances"`
	}
	_ = json.Unmarshal(data, &resp)
	out := resp.Instances[:0]
	for _, a := range resp.Instances {
		if a.RetiredAt != "" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// =============================================================================
// formatting helpers
// =============================================================================

func currentOrg(orgs []daemonOrgEntry) *daemonOrgEntry {
	for i := range orgs {
		if orgs[i].IsCurrent {
			return &orgs[i]
		}
	}
	if len(orgs) > 0 {
		return &orgs[0]
	}
	return nil
}

func currentOrgID(orgs []daemonOrgEntry) string {
	if c := currentOrg(orgs); c != nil {
		return c.ID
	}
	return ""
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// humanDuration prints a sub-second-precision duration suited to "uptime"
// — minutes for the first hour, then "1h 23m", then "2d 4h", etc.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		return fmt.Sprintf("%dh %dm", h, m)
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) - days*24
	return fmt.Sprintf("%dd %dh", days, h)
}

// humanLastSeen renders an RFC3339 timestamp as "5m ago" / "3h ago" /
// "2d ago" / a literal date for old timestamps. Empty/unparseable input
// renders as "never".
func humanLastSeen(ts string) string {
	if ts == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t)
	if d < 0 {
		return "just now"
	}
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	if d < 30*24*time.Hour {
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
	return t.Format("2006-01-02")
}
