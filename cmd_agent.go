//go:build darwin || linux

package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// runAgentAttach connects to the per-instance daemon socket and
// shuffles bytes both directions. The local TTY is put into raw
// mode for the duration so escape sequences pass through cleanly;
// the original termios is captured + restored on exit.
//
// Ctrl-] (0x1d) detaches: drops the connection without affecting
// the agent process. Same prefix as telnet/screen escape — close
// enough to muscle memory that I don't need to invent a new one.
func runAgentAttach(args []string) {
	// Go's flag.Parse stops at the first non-flag positional, so
	// `hearth hh agent attach <id> --write` would treat --write as a
	// second positional and reject. Pre-split flags from positionals
	// so flag order doesn't matter.
	write := false
	var positional []string
	for _, a := range args {
		switch a {
		case "--write", "-write":
			write = true
		case "--help", "-h":
			fmt.Fprintln(os.Stderr, "Usage: hearth hh agent attach <instance-id> [--write]")
			return
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "hearth hh agent attach: unknown flag %q\n", a)
				os.Exit(1)
			}
			positional = append(positional, a)
		}
	}
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: hearth hh agent attach <instance-id> [--write]")
		os.Exit(1)
	}
	instanceID := positional[0]
	sockPath := fmt.Sprintf("/tmp/hearth-attach-%d-%s.sock", os.Getuid(), instanceID)

	conn, err := net.DialTimeout("unix", sockPath, 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth hh agent attach: cannot connect to %s: %v\n"+
			"Is the agent running on this host? Today only claude agents support attach.\n",
			sockPath, err)
		os.Exit(1)
	}
	defer conn.Close()

	handshake := "ATTACH ro\n"
	if write {
		handshake = "ATTACH rw\n"
	}
	if _, err := conn.Write([]byte(handshake)); err != nil {
		fmt.Fprintf(os.Stderr, "hearth hh agent attach: handshake write: %v\n", err)
		os.Exit(1)
	}
	br := bufio.NewReader(conn)
	ack, err := br.ReadString('\n')
	if err != nil || strings.TrimSpace(ack) != "OK" {
		fmt.Fprintf(os.Stderr, "hearth hh agent attach: handshake failed: %q (err=%v)\n", ack, err)
		os.Exit(1)
	}

	// Save + restore terminal state. stty -g is the portable form;
	// no x/term dependency required.
	restore, err := makeRawTTY()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearth hh agent attach: cannot enter raw mode: %v\n", err)
		os.Exit(1)
	}
	mode := "read-only"
	if write {
		mode = "WRITE"
	}

	// Reserve the bottom row of the terminal as a status strip we
	// own. Set DECSTBM scroll region to rows 1..(rows-1) so claude's
	// scrolling stays above it; redraw the bar periodically (a
	// ticker + after every chunk we pass through) so a screen
	// clear from claude only steals it for a frame. SIGWINCH
	// re-measures + reconfigures. See statusBar for the details.
	bar := newStatusBar(fmt.Sprintf("hearth attach %s [%s] — Ctrl-] to detach",
		instanceID, mode))
	bar.start()

	// Deferred cleanup order matters: stop the status bar (clears
	// the row and restores the scroll region), restore termios so
	// the user's shell can echo again, then run `reset` to undo
	// any alt-screen / cursor-hidden / weird-SGR state the TUI
	// left behind. Restoring the title is a courtesy — most
	// terminals will reassert their own on the next prompt cycle.
	defer func() {
		bar.stop()
		restore()
		setTerminalTitle("")
		runReset()
	}()

	// Title bar also gets the hint as a belt-and-suspenders signal
	// for terminals (or operators) whose attention is up there.
	setTerminalTitle(fmt.Sprintf("hearth attach %s [%s] — Ctrl-] to detach", instanceID, mode))

	// Tiny preamble printed via stderr — goes into scrollback as
	// a record of when the attach happened.
	fmt.Fprintf(os.Stderr, "[hearth attach %s, %s — Ctrl-] to detach]\r\n", instanceID, mode)

	// socket → stdout, draining any buffered handshake bytes first.
	// After each chunk we redraw the status bar so claude can't
	// keep it permanently hidden (e.g. via screen clear); the bar's
	// own ticker handles idle periods.
	socketDone := make(chan struct{})
	go func() {
		defer close(socketDone)
		buf := make([]byte, 4096)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				_, _ = os.Stdout.Write(buf[:n])
				bar.draw()
			}
			if err != nil {
				return
			}
		}
	}()

	// stdin → socket. Watch for Ctrl-] (0x1d) to detach. In read-only
	// mode we still consume stdin so Ctrl-] works, but drop everything
	// else on the floor (don't send to socket).
	stdin := bufio.NewReader(os.Stdin)
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		buf := make([]byte, 256)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				// Scan for the detach key.
				escIdx := -1
				for i := 0; i < n; i++ {
					if buf[i] == 0x1d {
						escIdx = i
						break
					}
				}
				if escIdx >= 0 {
					// Forward anything before the escape (write mode
					// only) and then close.
					if write && escIdx > 0 {
						_, _ = conn.Write(buf[:escIdx])
					}
					return
				}
				if write {
					if _, werr := conn.Write(buf[:n]); werr != nil {
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Whichever direction closes first ends the session. The local
	// closes are clean (detach); socket close means the agent exited
	// or was killed.
	select {
	case <-socketDone:
		fmt.Fprintln(os.Stderr, "\r\n[hearth attach: agent disconnected]")
	case <-stdinDone:
		fmt.Fprintln(os.Stderr, "\r\n[hearth attach: detached]")
	}
}

// makeRawTTY sets the controlling terminal to raw mode via stty,
// returning a closure that restores the previous state. Uses
// `stty -g` to capture the full termios as a portable opaque
// string — works identically on macOS and Linux. Skips both calls
// when stdin isn't a TTY (e.g. piped input during tests).
func makeRawTTY() (func(), error) {
	if !isTTY(os.Stdin) {
		return func() {}, nil
	}
	saveOut, err := runStty("-g")
	if err != nil {
		return nil, fmt.Errorf("stty -g: %w", err)
	}
	saved := strings.TrimSpace(saveOut)
	if _, err := runStty("raw", "-echo"); err != nil {
		return nil, fmt.Errorf("stty raw: %w", err)
	}
	return func() {
		_, _ = runStty(saved)
	}, nil
}

func runStty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return string(out), err
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// setTerminalTitle emits the OSC 2 escape sequence to set the
// terminal window title. Empty string clears (writes a space, which
// most terminals render as blank). Best-effort — no-op if stderr
// isn't a TTY.
func setTerminalTitle(title string) {
	if !isTTY(os.Stderr) {
		return
	}
	if title == "" {
		title = " "
	}
	fmt.Fprintf(os.Stderr, "\x1b]2;%s\x07", title)
}

// runReset shells out to `reset` after detach to undo any
// alt-screen / cursor-hidden / mouse-tracking / weird-SGR state
// the agent's TUI left behind. The termios restore handled by
// `stty <saved>` above covers raw mode separately; this addresses
// the visual-state half of "wonky terminal after attach". Both
// macOS BSD `reset` and ncurses `reset` are tolerable here.
func runReset() {
	cmd := exec.Command("reset")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

// statusBar reserves the bottom row of the terminal for a static
// hint ("Ctrl-] to detach"). Implementation:
//
//   - DECSTBM scroll region set to rows 1..(rows-1) so the TUI's
//     scrolling never reaches our row. Claude's spawn-time PTY
//     size is 40x120 and we don't propagate resize (design pick),
//     so its TUI rendering stays in rows 1..40 of the local
//     terminal regardless of our actual height.
//   - Status text drawn at row N with cursor save/restore (DECSC/
//     DECRC) so the TUI's cursor isn't disturbed.
//   - A ticker redraws every 500ms in case a screen-clear from the
//     TUI wipes the row (DECSTBM constrains scrolling, not clear-
//     screen). A redraw after every chunk we pass through (in the
//     socket→stdout loop) makes recovery near-instant during
//     active output.
//   - SIGWINCH re-measures via `stty size` and reconfigures.
//   - Stop() clears the row and resets the scroll region to the
//     full screen so the post-detach `reset` doesn't fight us.
type statusBar struct {
	mu      sync.Mutex
	text    string
	rows    int
	cols    int
	stopCh  chan struct{}
	stopped bool
}

func newStatusBar(text string) *statusBar {
	return &statusBar{text: text, stopCh: make(chan struct{})}
}

// start measures the terminal, configures the scroll region, and
// begins the periodic redraw + SIGWINCH-watcher goroutines. Safe
// to call once; stop() must be called before exit.
func (b *statusBar) start() {
	b.measureAndConfigure()
	b.draw()

	// Periodic redraw — recovers from screen clears.
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				b.draw()
			case <-b.stopCh:
				return
			}
		}
	}()

	// SIGWINCH — local terminal resized. We don't propagate to the
	// PTY (design pick), so the TUI keeps rendering at 40x120; we
	// just re-pin the status row at the new bottom.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGWINCH)
		defer signal.Stop(sigCh)
		for {
			select {
			case <-sigCh:
				b.measureAndConfigure()
				b.draw()
			case <-b.stopCh:
				return
			}
		}
	}()
}

// stop clears the status row, resets the scroll region to the full
// screen, and signals the background goroutines to exit. Idempotent.
func (b *statusBar) stop() {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	b.stopped = true
	rows := b.rows
	b.mu.Unlock()
	close(b.stopCh)
	if rows > 0 {
		// Clear status row, reset scroll region, leave cursor at top.
		fmt.Fprintf(os.Stderr, "\x1b[%d;1H\x1b[K\x1b[r", rows)
	}
}

// draw paints the status text at the bottom row, surrounded by
// cursor save/restore so the TUI's cursor position isn't lost.
// No-op when rows isn't known yet (pre-measure) or after stop().
func (b *statusBar) draw() {
	b.mu.Lock()
	rows, cols, text, stopped := b.rows, b.cols, b.text, b.stopped
	b.mu.Unlock()
	if stopped || rows <= 0 {
		return
	}
	// Truncate text to fit if narrower than the bar string.
	if cols > 0 && len(text) > cols {
		text = text[:cols]
	}
	// DECSC (save) + position + clear-line + write + DECRC (restore).
	// Inverse video (7) so the bar stands apart from the TUI.
	fmt.Fprintf(os.Stderr, "\x1b7\x1b[%d;1H\x1b[K\x1b[7m%s\x1b[0m\x1b8", rows, text)
}

// measureAndConfigure reads the local terminal size via `stty size`
// and sets the DECSTBM scroll region to rows 1..(rows-1), reserving
// the last row for the status bar. Best-effort — failures leave
// rows==0 which makes draw/stop no-op.
func (b *statusBar) measureAndConfigure() {
	rows, cols, err := getTerminalSize()
	if err != nil || rows < 2 {
		return
	}
	b.mu.Lock()
	b.rows = rows
	b.cols = cols
	b.mu.Unlock()
	// Set scroll region [1, rows-1] and home the cursor.
	fmt.Fprintf(os.Stderr, "\x1b[1;%dr\x1b[H", rows-1)
}

func getTerminalSize() (rows, cols int, err error) {
	out, err := runStty("size")
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(strings.TrimSpace(out))
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unexpected stty size output: %q", out)
	}
	r, err1 := strconv.Atoi(parts[0])
	c, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("stty size parse: %v / %v", err1, err2)
	}
	return r, c, nil
}
