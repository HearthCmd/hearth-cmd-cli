//go:build darwin || linux

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// version is set at build time via -ldflags "-X main.version=..."
var version string

// wsURL is the relay server URL, set at build time via:
//
//	# dev
//	go build -ldflags "-X main.wsURL=wss://api.hearthcmd.dev/ws/relay" -o hearth .
//	# prod
//	go build -ldflags "-X main.wsURL=wss://api.hearthcmd.com/ws/relay" -o hearth .
//
// scripts/build.sh cross-compiles for every supported platform and
// embeds the right URL per env.
var wsURL string

// updateURL overrides the binary download URL for updates, set at build time via:
//
//	go build -ldflags "-X main.updateURL=file:///path/to/binary" -o hearth .
//
// When set, skips GitHub version checking and downloads from this URL directly.
// Useful for local testing of the update flow.
var updateURL string

func main() {
	// Log to file to avoid polluting the terminal (which may be in raw mode)
	if logPath := os.Getenv("HEARTH_LOG"); logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			log.SetOutput(f)
		}
	} else {
		logPath = filepath.Join(os.TempDir(), fmt.Sprintf("hearth-%d.log", os.Getpid()))
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			log.SetOutput(f)
		}
	}

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "stream":
		runStream(os.Args[2:])
	case "login":
		runRegister(os.Args[2:])
	case "logout":
		runLogout(os.Args[2:])
	case "start":
		runStart(os.Args[2:])
	case "stop":
		runStop(os.Args[2:])
	case "status":
		runStatus(os.Args[2:])
	case "hh":
		runOrganization(os.Args[2:])
	case "wd":
		runWD(os.Args[2:])
	case "talk":
		runTalk(os.Args[2:])
	case "resource":
		runResource(os.Args[2:])
	case "run":
		runRun(os.Args[2:])
	case "secret":
		runSecret(os.Args[2:])
	case "chat":
		runChat(os.Args[2:])
	case "plugin":
		runPlugin(os.Args[2:])
	case "update":
		runUpdate(os.Args[2:])
	case "version", "--version", "-v":
		printVersion()
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "hearth: unknown command %q\nRun 'hearth help' for usage.\n", os.Args[1])
		os.Exit(1)
	}
}

func versionString() string {
	v := version
	if v == "" {
		v = "dev"
	}
	return fmt.Sprintf("hearth %s", v)
}

func printVersion() {
	fmt.Fprintln(os.Stderr, versionString())
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `%s

Usage: hearth <command> [flags]

Commands:
  login      Log in with your email and enroll this host
  start      Start the daemon on this host
  stop       Stop the daemon on this host
  status     Show daemon status on this host
  logout     Sign out on this machine (stops the host, revokes creds, clears config)

  hh         Manage household entities (household, user, job_description, position, agent, ai_model, host, device, invite)
  wd         Manage working directories (list, get, create, update, abandon, delete)

  talk       Open a TUI to talk to your active agent instances

  resource   Invoke verbs on installed resource plugins (invoke)
  run        Run a command with IAM-gated secrets injected as env vars
  secret     Manage host-pinned plugin credentials (list, set, delete)
  plugin     Install/uninstall/list resource plugins on this host

  update     Update hearth to the latest version
  version    Print version and build settings

Run 'hearth <command> --help' for details on a command.
`, versionString())
}
