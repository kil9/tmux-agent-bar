package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var stateDir = "/tmp/tmux-agent-bar"

// procRoot is the root of the proc pseudo-filesystem. Overridable in tests.
var procRoot = "/proc"

// thinkingTTL is the maximum duration a "thinking" state is retained before it
// is treated as stale. Handles Claude Code killed without firing the Stop hook.
const thinkingTTL = 2 * time.Hour

func main() {
	// Hard deadline: must finish within one status-interval (1 s).
	// On timeout, print a fallback so the status bar stays informative.
	subcmd := ""
	if len(os.Args) >= 2 {
		subcmd = os.Args[1]
	}
	// Only the status-bar rendering subcommands run under the hard deadline: they
	// are spawned every status-interval and must never hang the bar. install is a
	// one-shot command that legitimately takes longer and rewrites
	// ~/.claude/settings.json; killing it mid-write could corrupt that file (and
	// drop every hook), so it must not be subject to the 900ms watchdog.
	switch subcmd {
	case "status", "claude-right", "hook":
		go func() {
			time.Sleep(900 * time.Millisecond)
			switch subcmd {
			case "status":
				// ⌛ = "status lookup timed out". Distinct from bg_waiting's ⏳.
				fmt.Print("⌛")
			case "claude-right":
				fmt.Printf("#[fg=colour66,bg=colour234]\ue0ba")
			}
			os.Exit(124)
		}()
	}

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tmux-agent-bar <hook|status|claude-right|install> [args...]")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "hook":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: tmux-agent-bar hook <thinking|waiting|done|error|subagent_stop|planning|bg_waiting|session_end>")
			os.Exit(1)
		}
		runHook(os.Args[2])
	case "status":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: tmux-agent-bar status <window_index>")
			os.Exit(1)
		}
		runStatus(os.Args[2])
	case "claude-right":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: tmux-agent-bar claude-right <pane_id>")
			os.Exit(1)
		}
		runClaudeRight(os.Args[2])
	case "install":
		runInstall()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		os.Exit(1)
	}
}

// runHook writes the status for the current tmux pane.
// Called from Claude Code hooks.
func runHook(status string) {
	switch status {
	case "thinking", "waiting", "done", "error", "subagent_stop", "planning", "bg_waiting", "session_end":
		// valid
	default:
		fmt.Fprintf(os.Stderr, "invalid status: %s (must be thinking|waiting|done|error|subagent_stop|planning|bg_waiting|session_end)\n", status)
		os.Exit(1)
	}

	// TMUX_PANE is set when inside a tmux pane (e.g. %3).
	// Check before reading stdin to avoid unnecessary blocking outside tmux.
	paneID := os.Getenv("TMUX_PANE")
	if paneID == "" {
		// Not inside tmux — silently exit.
		return
	}

	// Read hook stdin — stdin can only be read once.
	hookData, _ := parseHookStdin()

	session, windowIndex, pane, err := tmuxPaneParts(paneID)
	if err != nil {
		// tmux unavailable or pane lookup failed (e.g. TMUX_PANE set by a
		// tmux-clone like psmux that doesn't support display-message).
		// Treat the same as "not in tmux" and exit silently.
		return
	}
	key := stateKey(session, windowIndex, pane)

	// SessionEnd: Claude Code exited (or /clear started a fresh session) — drop
	// every file for this pane immediately so stale state/meta doesn't linger
	// until the pane closes.
	if status == "session_end" {
		clearPaneFiles(key)
		return
	}

	// SubagentStop: record a timestamp (used only for cleanup; no longer needed
	// to suppress Notification since deferred notify handles it).
	if status == "subagent_stop" {
		_ = markSubagentStop(key)
		return
	}

	// Notification hook: write a deferred marker instead of immediately setting
	// state. runStatus promotes it to "waiting" only after notifyPendingDelay has
	// elapsed without a Stop/thinking hook clearing it. This prevents 💬 from
	// flashing when Notification fires just before Stop (normal end-of-response).
	if status == "waiting" {
		_ = writeNotifyPending(key)
		if meta, ok := resolvePaneMeta(hookData); ok {
			_ = writeMeta(key, meta)
		}
		if panes, err := tmuxListPanes(session, windowIndex); err == nil {
			cleanStaleFiles(stateDir, session, windowIndex, panes)
		}
		return
	}

	// For all non-waiting statuses: clear any pending notify marker so that a
	// Notification that arrived before this hook does not promote to 💬.
	os.Remove(filepath.Join(stateDir, key+".notify_pending"))

	// When in "thinking" state, ensure the start-time marker exists so the
	// elapsed-time counter stays stable across repeated PreToolUse calls.
	// Check the marker file itself (not the state file) so that a stale
	// "thinking" state left over from a previous session still gets a fresh
	// marker.  When leaving thinking, remove the marker.
	if status == "thinking" {
		if _, ok := readThinkingStart(key); !ok {
			_ = writeThinkingStart(key)
		}
	} else {
		os.Remove(filepath.Join(stateDir, key+".thinking_start"))
	}

	// When entering plan mode, record a marker so that the subsequent Stop
	// (done) hook can promote to 💬 (waiting for user approval) instead of ✅.
	if status == "planning" {
		_ = writePlanPending(key)
	}

	// When Stop fires after a plan was presented, show 💬 instead of ✅ so
	// the user knows their approval is needed. Consume the marker here.
	//
	// Background-job detection (⏳) is deliberately NOT done here: at Stop
	// time the hook process itself — and any sibling Stop hooks — are children
	// of claude, so "claude has a live child" is ambiguous. runStatus resolves
	// done→bg_waiting at render time instead; the visible latency is the same
	// since tmux re-runs #() only every status-interval anyway.
	if status == "done" && planPendingExists(key) {
		os.Remove(filepath.Join(stateDir, key+".plan_pending"))
		status = "waiting"
	}

	// Only rewrite the state file when the status actually changes.
	// Repeated PreToolUse ("thinking") calls must not update the mtime,
	// which is used as a fallback elapsed-time source.
	if readState(key) != status {
		if err := writeState(key, status); err != nil {
			fmt.Fprintln(os.Stderr, "tmux-agent-bar hook: failed to write state:", err)
			os.Exit(1)
		}
	}

	// Update pane metadata (model + context usage) from hook stdin / transcript.
	if meta, ok := resolvePaneMeta(hookData); ok {
		_ = writeMeta(key, meta) // best-effort
	}

	// Best-effort: remove state files for panes that no longer exist.
	if panes, err := tmuxListPanes(session, windowIndex); err == nil {
		cleanStaleFiles(stateDir, session, windowIndex, panes)
	}
}

// paneFileSuffixes lists every marker-file suffix stored alongside a pane's
// state file in stateDir.
var paneFileSuffixes = []string{".meta", ".thinking_start", ".subagent_stop", ".notify_pending", ".plan_pending"}

// clearPaneFiles removes the state file and all marker files for the given key.
func clearPaneFiles(key string) {
	os.Remove(filepath.Join(stateDir, key))
	for _, suffix := range paneFileSuffixes {
		os.Remove(filepath.Join(stateDir, key+suffix))
	}
}

// cleanStaleFiles removes state files for closed panes in the given session/window.
// dir specifies the state directory (use stateDir in production, t.TempDir() in tests).
func cleanStaleFiles(dir, session, windowIndex string, alivePanes []string) {
	alive := make(map[string]bool, len(alivePanes))
	for _, p := range alivePanes {
		alive[stateKey(session, windowIndex, p)] = true
	}

	prefix := session + "_" + windowIndex + "_"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		// Match state files and all marker files (.meta, .thinking_start, .subagent_stop).
		base := name
		for _, suffix := range paneFileSuffixes {
			base = strings.TrimSuffix(base, suffix)
		}
		if strings.HasPrefix(base, prefix) && !alive[base] {
			os.Remove(filepath.Join(dir, name))
		}
	}
}

// orphanGCInterval bounds how often cleanOrphanState scans the state
// directory, regardless of how many windows/ticks call it.
const orphanGCInterval = 5 * time.Minute

// cleanOrphanState removes state files whose (session, window) no longer exists.
// cleanStaleFiles only reaps closed panes within a window that is still being
// rendered; when a whole window or session is torn down, runStatus never
// iterates it again, so its files would otherwise linger until /tmp is cleared.
// Throttled via a marker file so the directory scan runs at most once per
// orphanGCInterval.
func cleanOrphanState() {
	marker := filepath.Join(stateDir, ".gc")
	if info, err := os.Stat(marker); err == nil && time.Since(info.ModTime()) < orphanGCInterval {
		return
	}

	windowKeys, err := tmuxListWindowKeys()
	if err != nil || len(windowKeys) == 0 {
		// tmux unavailable or returned nothing — never risk deleting live state.
		return
	}

	// Touch the marker before scanning so a slow/failing scan doesn't run every tick.
	_ = os.WriteFile(marker, nil, 0o644)
	removeOrphanFiles(stateDir, windowKeys)
}

// removeOrphanFiles deletes every entry in dir whose name does not belong to one
// of the live windows. A file belongs to a window when its name starts with
// "<session>_<window>_". Dotfiles (e.g. the .gc marker) are skipped. Pure
// side-effect on the filesystem so it can be unit-tested without tmux.
func removeOrphanFiles(dir string, liveWindowKeys []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		live := false
		for _, k := range liveWindowKeys {
			if strings.HasPrefix(name, k+"_") {
				live = true
				break
			}
		}
		if !live {
			os.Remove(filepath.Join(dir, name))
		}
	}
}

// tmuxListWindowKeys returns "<session>_<window>" for every live window across
// all sessions — the prefix (minus the trailing pane component) of the state
// file keys produced by stateKey.
func tmuxListWindowKeys() ([]string, error) {
	out, err := tmuxCommand("list-windows", "-a", "-F", "#S_#I")
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if k := strings.TrimSpace(line); k != "" {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// formatElapsed renders a thinking elapsed time for the status bar at
// minute-or-coarser granularity. Seconds are dropped entirely and minutes are
// floored, so anything under one minute returns "" (nothing is shown).
//   - <60s        -> "" (hidden)
//   - <3600s      -> "5m"
//   - >=3600s     -> "1h5m" (the "0m" is kept, e.g. "1h0m")
func formatElapsed(seconds int) string {
	if seconds < 60 {
		return ""
	}
	if seconds >= 3600 {
		return fmt.Sprintf("%dh%dm", seconds/3600, (seconds%3600)/60)
	}
	return fmt.Sprintf("%dm", seconds/60)
}

// runStatus prints the emoji for the given window index.
// Called from tmux window-status-format every status-interval.
func runStatus(windowIndex string) {
	session, err := tmuxCurrentSession()
	if err != nil {
		// Outside tmux or tmux unavailable — print nothing.
		return
	}

	// Opportunistically reap state files from windows/sessions that no longer
	// exist. Throttled internally, so this is cheap to call on every status tick.
	cleanOrphanState()

	emoji := aggregateWindowEmoji(session, windowIndex)

	// If the window shows ✅ and the user has activated (is currently viewing) it,
	// clear the done state so the check disappears.
	if emoji == "✅" {
		if activeWindow, err := tmuxCurrentWindowIndex(); err == nil && activeWindow == windowIndex {
			clearViewedStates(session, windowIndex)
			emoji = ""
		}
	}

	// For long-running states, append elapsed time in dimmed color.
	switch emoji {
	case "🤖":
		if start, ok := thinkingStartTime(session, windowIndex); ok {
			emoji += elapsedSuffix(start)
		}
	case "⏳":
		if start, ok := bgWaitingStartTime(session, windowIndex); ok {
			emoji += elapsedSuffix(start)
		}
	}

	fmt.Print(emoji)
}

// elapsedSuffix renders the dimmed "(elapsed)" suffix appended to the 🤖 and ⏳
// emojis. Returns "" when the elapsed time is under the display threshold (see
// formatElapsed), so nothing is shown for the first minute.
func elapsedSuffix(start time.Time) string {
	if timeStr := formatElapsed(int(time.Since(start).Seconds())); timeStr != "" {
		return fmt.Sprintf("#[fg=colour8](%s)#[fg=default]", timeStr)
	}
	return ""
}

// runClaudeRight outputs a tmux-format prefix for the status-right that includes:
//   - when Claude Code is active: a ctx+model segment (bg=colour241) followed by
//     the powerline separator transitioning into the date segment (bg=colour66)
//   - when inactive: just the powerline separator into the date segment
//
// Called from tmux status-right via #(tmux-agent-bar claude-right #{pane_id}).
// The caller's format string must continue with the date content on bg=colour66.
func runClaudeRight(paneID string) {
	const (
		sep    = ""         // powerline left-pointing solid triangle (U+E0BA)
		ctxBg  = "colour241" // context+model segment background
		dateBg = "colour66"  // date segment background (steel teal)
	)

	// inactive: no ctx segment — just the transition into the date segment.
	inactive := func() { fmt.Printf("#[fg=%s,bg=colour234]%s", dateBg, sep) }

	session, windowIndex, pane, err := tmuxPaneParts(paneID)
	if err != nil {
		inactive()
		return
	}
	key := stateKey(session, windowIndex, pane)
	meta, ok := readMeta(key)
	if !ok || meta.Model == "" {
		inactive()
		return
	}
	// The meta file can outlive the Claude Code session (SessionEnd doesn't
	// fire on kill -9 etc.), so verify a claude process is actually alive in
	// the pane before showing ctx%. Drop the stale meta when it isn't.
	//
	// Only trust an empty descendant set when the proc tree is actually
	// walkable. On macOS (no /proc) and Linux kernels without
	// CONFIG_PROC_CHILDREN, findClaudeDescendants always returns nothing, so
	// running the guard there would wrongly delete the meta on every render and
	// hide ctx% forever. When the proc tree is unavailable we trust the meta.
	if procTreeAvailable() {
		if pid, err := tmuxPanePID(paneID); err == nil && len(findClaudeDescendants(pid, bgWalkDepth)) == 0 {
			os.Remove(filepath.Join(stateDir, key+".meta"))
			inactive()
			return
		}
	}

	pct := meta.InputTokens * 100 / contextLimit()
	if pct > 100 {
		pct = 100
	}
	shortName := shortModelName(meta.Model)

	// ctx+model segment, then separator transitioning into the date segment.
	// colour121 (light green) for context %, colour148 (yellow-green) for model name.
	fmt.Printf(
		"#[fg=%s,bg=colour234]%s#[fg=colour121,bg=%s] %d%% #[fg=colour148]%s #[fg=%s,bg=%s]%s",
		ctxBg, sep,
		ctxBg,
		pct, shortName,
		dateBg, ctxBg, sep,
	)
}

// paneTime pairs a pane index with the mtime of its state file.
type paneTime struct {
	index string
	mtime time.Time
}

// selectPaneStartTime picks the start time to display from a list of candidate
// panes (those sharing the displayed state, e.g. thinking or bg_waiting) and the
// index of the most-recently-activated pane.
//
//   - 0 candidate panes → false
//   - 1 candidate pane → its mtime
//   - multiple candidates + lastActive is one of them → that pane's mtime
//   - multiple candidates + lastActive not among them → earliest mtime
func selectPaneStartTime(candidates []paneTime, lastActive string) (time.Time, bool) {
	if len(candidates) == 0 {
		return time.Time{}, false
	}
	if len(candidates) == 1 {
		return candidates[0].mtime, true
	}
	for _, pt := range candidates {
		if pt.index == lastActive {
			return pt.mtime, true
		}
	}
	// Fallback: earliest mtime.
	earliest := candidates[0].mtime
	for _, pt := range candidates[1:] {
		if pt.mtime.Before(earliest) {
			earliest = pt.mtime
		}
	}
	return earliest, true
}

// thinkingStartTime returns the mtime of the state file for the pane whose
// thinking elapsed time should be displayed:
//
//   - If exactly one pane is thinking, return its mtime.
//   - If multiple panes are thinking, return the mtime of the most recently
//     activated pane (by tmux pane_last_activity) that is in thinking state.
//     Falls back to the earliest mtime if the last-active pane is not thinking.
func thinkingStartTime(session, windowIndex string) (time.Time, bool) {
	panes, err := tmuxListPanes(session, windowIndex)
	if err != nil {
		return time.Time{}, false
	}

	var thinking []paneTime
	for _, pane := range panes {
		key := stateKey(session, windowIndex, pane)
		if readState(key) != "thinking" {
			continue
		}
		// Prefer the dedicated start-time marker (set when thinking first began,
		// not reset on each tool use); fall back to state file mtime.
		var startTime time.Time
		if t, ok := readThinkingStart(key); ok {
			startTime = t
		} else {
			path := filepath.Join(stateDir, key)
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			startTime = info.ModTime()
		}
		thinking = append(thinking, paneTime{index: pane, mtime: startTime})
	}

	lastActive, _ := tmuxLastActivePaneIndex(session, windowIndex)
	return selectPaneStartTime(thinking, lastActive)
}

// bgWaitingStartTime returns the time from which the ⏳ (bg_waiting) elapsed
// counter should run for the given window. When multiple panes wait, selection
// mirrors thinkingStartTime (last-active, else earliest).
func bgWaitingStartTime(session, windowIndex string) (time.Time, bool) {
	panes, err := tmuxListPanesWithCreated(session, windowIndex)
	if err != nil {
		return time.Time{}, false
	}

	var waiting []paneTime
	for _, pane := range panes {
		// Resolve render-time: a pane displays ⏳ while its recorded "done"
		// (written once at Stop, never rewritten while the job runs) has a
		// live background job, so that file's mtime marks when waiting began.
		if resolvePaneStateOrClear(session, windowIndex, pane) != "bg_waiting" {
			continue
		}
		key := stateKey(session, windowIndex, pane.index)
		info, err := os.Stat(filepath.Join(stateDir, key))
		if err != nil {
			continue
		}
		waiting = append(waiting, paneTime{index: pane.index, mtime: info.ModTime()})
	}

	lastActive, _ := tmuxLastActivePaneIndex(session, windowIndex)
	return selectPaneStartTime(waiting, lastActive)
}

// stateKey returns the state file name for a given session, window, and pane.
func stateKey(session, windowIndex, pane string) string {
	return session + "_" + windowIndex + "_" + pane
}

// emojiForStates returns the highest-priority emoji for the given slice of state strings.
//
// Priority: 🚨 (any error) > 💬 (any waiting) > ⏸ (any planning) > 🤖 (any thinking) > ⏳ (any bg_waiting) > ✅ (any done) > "" (all idle)
func emojiForStates(states []string) string {
	anyError := false
	anyWaiting := false
	anyPlanning := false
	anyThinking := false
	anyBgWaiting := false
	anyDone := false

	for _, s := range states {
		switch s {
		case "error":
			anyError = true
		case "waiting":
			anyWaiting = true
		case "planning":
			anyPlanning = true
		case "thinking":
			anyThinking = true
		case "bg_waiting":
			anyBgWaiting = true
		case "done":
			anyDone = true
		}
	}

	switch {
	case anyError:
		return "🚨"
	case anyWaiting:
		return "💬"
	case anyPlanning:
		return "⏸"
	case anyThinking:
		return "🤖"
	case anyBgWaiting:
		return "⏳"
	case anyDone:
		return "✅"
	default:
		return ""
	}
}

// tmuxCommand runs a tmux subcommand with a 200ms timeout.
// If the tmux server is slow or unresponsive, the command is killed
// rather than blocking the caller (and, transitively, tmux itself).
func tmuxCommand(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	return exec.CommandContext(ctx, "tmux", args...).Output()
}

// tmuxPaneParts returns the session, window index, and pane index for the given
// pane ID. It queries tmux with a tab-separated format so the three components
// can be read directly, rather than reparsing a "<session>_<window>_<pane>"
// key with SplitN — which breaks when the session name itself contains "_".
// Assemble the state file key from the returned values via stateKey.
func tmuxPaneParts(paneID string) (session, windowIndex, pane string, err error) {
	out, err := tmuxCommand("display-message", "-p", "-t", paneID, "#S\t#I\t#P")
	if err != nil {
		return "", "", "", err
	}
	return parsePaneParts(string(out))
}

// parsePaneParts splits tmux's tab-separated "#S\t#I\t#P" output into its
// session, window, and pane components. Extracted from tmuxPaneParts so the
// parsing can be unit-tested without invoking tmux. Session names may contain
// "_" (or any character except tab), so a tab separator is unambiguous.
func parsePaneParts(raw string) (session, windowIndex, pane string, err error) {
	fields := strings.SplitN(strings.TrimSpace(raw), "\t", 3)
	if len(fields) != 3 {
		return "", "", "", fmt.Errorf("unexpected pane format: %q", raw)
	}
	return fields[0], fields[1], fields[2], nil
}

// tmuxCurrentWindowIndex returns the index of the currently active window.
func tmuxCurrentWindowIndex() (string, error) {
	out, err := tmuxCommand("display-message", "-p", "#I")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// clearViewedStates removes state files for panes in the given window that are
// in "done" state. Called when the user activates the window so that ✅
// disappears once seen. 💬 is intentionally not cleared here — it should
// persist until the user actually interacts (approves/types), at which point
// the next hook (PreToolUse → thinking, or Stop → done) overwrites it.
func clearViewedStates(session, windowIndex string) {
	panes, err := tmuxListPanes(session, windowIndex)
	if err != nil {
		return
	}
	for _, pane := range panes {
		key := stateKey(session, windowIndex, pane)
		if readState(key) == "done" {
			os.Remove(filepath.Join(stateDir, key))
		}
	}
}

// tmuxCurrentSession returns the name of the current tmux session.
func tmuxCurrentSession() (string, error) {
	out, err := tmuxCommand("display-message", "-p", "#S")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// paneCreated pairs a pane index with its creation timestamp and shell PID.
type paneCreated struct {
	index   string
	created time.Time
	pid     int
}

// tmuxListPanes returns all pane indices for the given session and window.
func tmuxListPanes(session, windowIndex string) ([]string, error) {
	panes, err := tmuxListPanesWithCreated(session, windowIndex)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(panes))
	for i, p := range panes {
		out[i] = p.index
	}
	return out, nil
}

// tmuxListPanesWithCreated returns pane indices together with their creation
// timestamps for the given session and window.
// It reads #{pane_created} first; if that is empty (some tmux builds don't
// populate it), it falls back to the pane PID's start time from /proc.
func tmuxListPanesWithCreated(session, windowIndex string) ([]paneCreated, error) {
	target := session + ":" + windowIndex
	raw, err := tmuxCommand("list-panes", "-t", target, "-F", "#{pane_index}\t#{pane_created}\t#{pane_pid}")
	if err != nil {
		return nil, err
	}
	var panes []paneCreated
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		pc := paneCreated{index: fields[0]}
		if len(fields) >= 2 {
			if ts, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64); err == nil && ts > 0 {
				pc.created = time.Unix(ts, 0)
			}
		}
		if len(fields) >= 3 {
			if pid, err := strconv.Atoi(strings.TrimSpace(fields[2])); err == nil && pid > 0 {
				pc.pid = pid
				// pane_created unavailable: fall back to pane PID start time.
				if pc.created.IsZero() {
					if t, ok := procStartTime(pid); ok {
						pc.created = t
					}
				}
			}
		}
		panes = append(panes, pc)
	}
	return panes, nil
}

// procStartTime returns the start time of the process with the given PID by
// reading /proc/<pid>/stat. Returns (zero, false) on any error or non-Linux systems.
func procStartTime(pid int) (time.Time, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}, false
	}
	// /proc/<pid>/stat fields are space-separated; field 22 (0-indexed: 21) is
	// starttime in clock ticks since boot. We also need /proc/stat btime (boot time).
	//
	// Format: pid (comm) state ppid ... starttime ...
	// The comm field may contain spaces and parentheses, so find the last ')' first.
	s := string(data)
	rp := strings.LastIndex(s, ")")
	if rp < 0 {
		return time.Time{}, false
	}
	rest := strings.TrimSpace(s[rp+1:])
	fields := strings.Fields(rest)
	// After stripping "pid (comm)", field index 19 is starttime (field 22 overall, 0-indexed 21).
	// Fields after ')': [state ppid pgrp session tty_nr tpgid flags minflt cminflt majflt cmajflt
	//                    utime stime cutime cstime priority nice num_threads itrealvalue starttime ...]
	// Index 19 = starttime (0-based after the ')' remainder).
	if len(fields) < 20 {
		return time.Time{}, false
	}
	startTicks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	// Read boot time from /proc/stat.
	btimeData, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	var btime int64
	for _, line := range strings.Split(string(btimeData), "\n") {
		if strings.HasPrefix(line, "btime ") {
			btime, _ = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
			break
		}
	}
	if btime == 0 {
		return time.Time{}, false
	}
	// USER_HZ is 100 on Linux/x86 (virtually always).
	const userHz = 100
	startSec := btime + startTicks/userHz
	return time.Unix(startSec, 0), true
}

// bgWalkDepth caps how deep the pane's process tree is searched for a claude
// process. The chain is often deeper than "shell → claude": an npm-installed
// claude runs as shell → node (launcher shim) → claude, and custom wrappers
// can add another level.
const bgWalkDepth = 4

// bgJobComms are the process names counted as Claude-spawned background jobs.
// Bash run_in_background / Monitor workers run as a persistent shell child of
// claude. Other long-lived children — MCP servers (docker, python, node, npm),
// statusline wrappers — are session infrastructure, not background work, and
// must not keep the pane stuck on ⏳.
var bgJobComms = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true}

// paneHasBackgroundJobs reports whether a claude process under the pane shell
// (panePID) still has a live shell child — i.e. Claude left background work
// running (Bash run_in_background, Monitor) after its response ended. This
// catches the case where Stop fired but the work is not actually done.
//
// Two guards keep session infrastructure from pinning panes to ⏳:
//   - only shell children (bgJobComms) count — MCP servers and statusline
//     wrappers run as node/docker/python and live for the whole session;
//   - shell-wrapped MCP servers are additionally filtered by cmdline
//     (looksLikeMCPServer).
//
// The calling process and its ancestors are excluded from the child check:
// when this runs from a Claude Code hook, the hook shell itself is a child
// of claude and must not count as a background job.
func paneHasBackgroundJobs(panePID int) bool {
	exclude := selfAncestors()
	for _, claudePID := range findClaudeDescendants(panePID, bgWalkDepth) {
		for _, child := range readProcChildren(claudePID) {
			if !bgJobComms[readProcComm(child)] || exclude[child] {
				continue
			}
			if looksLikeMCPServer(readProcCmdline(child)) {
				continue
			}
			return true
		}
	}
	return false
}

// findClaudeDescendants returns the PIDs of processes whose comm contains
// "claude" among the descendants of pid, searching at most maxDepth levels
// down. Descendants of a found claude process are not searched further — its
// children are its jobs, not more claude candidates.
func findClaudeDescendants(pid, maxDepth int) []int {
	var found []int
	frontier := []int{pid}
	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		var next []int
		for _, p := range frontier {
			for _, c := range readProcChildren(p) {
				if strings.Contains(readProcComm(c), "claude") {
					found = append(found, c)
					continue
				}
				next = append(next, c)
			}
		}
		frontier = next
	}
	return found
}

// selfAncestors returns the calling process's PID and its ancestors' PIDs.
func selfAncestors() map[int]bool {
	set := make(map[int]bool)
	pid := os.Getpid()
	for pid > 1 && !set[pid] {
		set[pid] = true
		pid = procPPID(pid)
	}
	return set
}

// procPPID returns the parent PID of pid from /proc/<pid>/stat, or 0 on error.
func procPPID(pid int) int {
	if pid <= 0 {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	// Skip past "pid (comm)" — comm may contain spaces and parentheses.
	s := string(data)
	rp := strings.LastIndex(s, ")")
	if rp < 0 {
		return 0
	}
	fields := strings.Fields(s[rp+1:]) // [state ppid pgrp ...]
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

// looksLikeMCPServer reports whether cmdline looks like a Model Context
// Protocol server that Claude keeps alive for the whole session rather than a
// background work process. Matched case-insensitively by either "mcp"
// (covers .../.ccs/mcp/...-server.cjs, uvx mcp-server-...) or the full
// "modelcontextprotocol" — the canonical npx @modelcontextprotocol/server-...
// form does NOT contain the substring "mcp", so it must be matched separately.
// Empty cmdline (proc gone / unreadable) is treated as not-an-MCP-server so
// genuine background jobs still register.
func looksLikeMCPServer(cmdline string) bool {
	lc := strings.ToLower(cmdline)
	return strings.Contains(lc, "mcp") || strings.Contains(lc, "modelcontextprotocol")
}

// readProcChildren returns the direct child PIDs of pid by reading
// /proc/<pid>/task/<pid>/children. Returns nil on any error or non-Linux
// systems (where the file doesn't exist).
func readProcChildren(pid int) []int {
	if pid <= 0 {
		return nil
	}
	pidStr := strconv.Itoa(pid)
	path := filepath.Join(procRoot, pidStr, "task", pidStr, "children")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return nil
	}
	children := make([]int, 0, len(fields))
	for _, f := range fields {
		if cpid, err := strconv.Atoi(f); err == nil && cpid > 0 {
			children = append(children, cpid)
		}
	}
	return children
}

// procTreeAvailable reports whether this system exposes the per-process
// children file the liveness guard relies on, judged by reading the calling
// process's own file. See procTreeAvailableIn for why the children file — not
// procRoot's existence — is the criterion.
func procTreeAvailable() bool {
	return procTreeAvailableIn(procRoot, os.Getpid())
}

// procTreeAvailableIn reports whether root exposes the children file for
// selfPID (root/<selfPID>/task/<selfPID>/children). This is the file
// findClaudeDescendants walks to enumerate a pane's descendants; when it is
// unreadable, that walk always yields nothing, which must NOT be read as
// "no claude alive". Two systems lack it: macOS has no /proc at all, and some
// Linux kernels are built without CONFIG_PROC_CHILDREN (so /proc exists but the
// children file does not) — hence the criterion is the file's readability, not
// procRoot's existence. Pulled out as a pure, root-parameterised helper so the
// guard-skip decision is unit-testable with a fixture proc tree.
func procTreeAvailableIn(root string, selfPID int) bool {
	if selfPID <= 0 {
		return false
	}
	pidStr := strconv.Itoa(selfPID)
	_, err := os.ReadFile(filepath.Join(root, pidStr, "task", pidStr, "children"))
	return err == nil
}

// readProcComm returns the command name (comm) for the given pid from
// /proc/<pid>/comm. Returns "" on any error.
func readProcComm(pid int) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readProcCmdline returns the full command line for pid from
// /proc/<pid>/cmdline, with the NUL argument separators replaced by spaces.
// Returns "" on any error (including a process that has already exited).
func readProcCmdline(pid int) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
}

// tmuxPanePID returns the pane_pid (the shell process PID) for the given
// pane ID. Used at Stop-hook time to inspect the descendant process tree.
func tmuxPanePID(paneID string) (int, error) {
	out, err := tmuxCommand("display-message", "-p", "-t", paneID, "#{pane_pid}")
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, err
	}
	return pid, nil
}

// tmuxLastActivePaneIndex returns the pane_index of the most recently activated
// pane in the given session/window, based on tmux's pane_last_activity timestamp.
func tmuxLastActivePaneIndex(session, windowIndex string) (string, error) {
	target := session + ":" + windowIndex
	out, err := tmuxCommand("list-panes", "-t", target, "-F", "#{pane_last_activity} #{pane_index}")
	if err != nil {
		return "", err
	}
	var latestTS int64
	var latestPane string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		ts, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			continue
		}
		if ts > latestTS {
			latestTS = ts
			latestPane = strings.TrimSpace(parts[1])
		}
	}
	if latestPane == "" {
		return "", fmt.Errorf("no panes found")
	}
	return latestPane, nil
}

// writeFileAtomic writes data to path via a temp file + rename so that a
// concurrent reader (the status tick) never observes a partially written file.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		if werr != nil {
			return werr
		}
		return cerr
	}
	return os.Rename(tmp.Name(), path)
}

// writePlanPending records that a plan was presented and is awaiting user approval.
func writePlanPending(key string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(stateDir, key+".plan_pending"), nil)
}

// planPendingExists reports whether a plan-pending marker exists for the given key.
func planPendingExists(key string) bool {
	_, err := os.Stat(filepath.Join(stateDir, key+".plan_pending"))
	return err == nil
}

// writeThinkingStart records the current time as the start of a thinking session.
// Called only when transitioning into thinking from a non-thinking state, so
// repeated PreToolUse calls during a single thinking session do not reset the clock.
func writeThinkingStart(key string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(stateDir, key+".thinking_start")
	return writeFileAtomic(path, []byte(time.Now().Format(time.RFC3339Nano)))
}

// readThinkingStart returns the recorded thinking start time for the given key.
func readThinkingStart(key string) (time.Time, bool) {
	data, err := os.ReadFile(filepath.Join(stateDir, key+".thinking_start"))
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// notifyPendingDelay is how long a .notify_pending marker must age before
// runStatus promotes the pane state to "waiting". Keeps 💬 from flashing
// during the brief Notification→Stop window at normal end-of-response.
const notifyPendingDelay = time.Second

// writeNotifyPending records the current time to the notify-pending marker.
// Called by "hook waiting" instead of writing state directly.
func writeNotifyPending(key string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(stateDir, key+".notify_pending")
	return writeFileAtomic(path, []byte(time.Now().Format(time.RFC3339Nano)))
}

// effectiveState returns the display state for the given pane.
// If the recorded state is "thinking" and a .notify_pending marker has aged
// past notifyPendingDelay without being cleared by a subsequent hook, the
// pane is promoted to "waiting". This defers 💬 long enough for Stop to fire
// and clear the marker in the common end-of-response case.
func effectiveState(key string, created time.Time) string {
	state := readStateFresh(key, created)
	if state != "thinking" {
		return state
	}
	info, err := os.Stat(filepath.Join(stateDir, key+".notify_pending"))
	if err != nil {
		return state
	}
	if time.Since(info.ModTime()) >= notifyPendingDelay {
		return "waiting"
	}
	return state
}

// markSubagentStop writes the current time to the subagent-stop marker file.
// Informational only — no state depends on it since deferred notify replaced
// the old suppression logic; it is cleaned up with the other pane files.
func markSubagentStop(key string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(stateDir, key+".subagent_stop")
	return writeFileAtomic(path, []byte(time.Now().Format(time.RFC3339Nano)))
}

// writeState writes the status string to the state file for the given key.
func writeState(key, status string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(stateDir, key)
	return writeFileAtomic(path, []byte(status))
}

// readState reads the status for the given key. Returns "" if the file doesn't exist.
func readState(key string) string {
	return readStateFresh(key, time.Time{})
}

// readStateFresh reads the status for the given key, but returns "" when the
// state file's mtime predates `after` (meaning the file was written in a
// previous pane lifetime and is now stale). Pass a zero time to skip the check.
// "thinking" state older than thinkingTTL is also treated as stale, so that a
// dead Claude Code session (Stop hook never fired) doesn't show 🤖 forever.
func readStateFresh(key string, after time.Time) string {
	path := filepath.Join(stateDir, key)
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if !after.IsZero() && info.ModTime().Before(after) {
		return "" // stale — written before this pane was created
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	state := strings.TrimSpace(string(data))
	if state == "thinking" {
		// Use thinking_start marker for accurate elapsed time; fall back to state file mtime.
		startTime := info.ModTime()
		if t, ok := readThinkingStart(key); ok {
			startTime = t
		}
		if time.Since(startTime) > thinkingTTL {
			return "" // expired — Claude Code was likely killed without Stop hook
		}
	}
	return state
}

// runInstall configures ~/.tmux.conf and ~/.claude/settings.json.
func runInstall() {
	errCount := 0
	if err := installTmuxConf(); err != nil {
		fmt.Fprintln(os.Stderr, "tmux.conf:", err)
		errCount++
	}
	if err := installClaudeSettings(); err != nil {
		fmt.Fprintln(os.Stderr, "settings.json:", err)
		errCount++
	}
	if errCount > 0 {
		os.Exit(1)
	}
}

// installTmuxConf appends tmux-agent-bar config to ~/.tmux.conf if not already present.
func installTmuxConf() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".tmux.conf")

	data, _ := os.ReadFile(path)
	content := string(data)

	marker := "tmux-agent-bar status"
	if strings.Contains(content, marker) {
		fmt.Println("~/.tmux.conf: already configured, skipping")
		return nil
	}

	addition := `
# tmux-agent-bar
# window format 의 #(tmux-agent-bar) 는 interval 마다 창 개수만큼 프로세스를 spawn 한다.
# 완료/주의 알림은 Claude Code 네이티브 벨(settings.json preferredNotifChannel:
# terminal_bell)이 즉시 담당하므로, 이모지 갱신은 굳이 촘촘히 폴링하지 않는다.
# interval 은 시계(%R) 가 멈추지 않을 만큼만 느슨하게 두고 idle 폴링 비용을 줄인다.
set -g status-interval 30
# 백그라운드 창에서 벨이 울리면 해당 창 이름을 강조해 시각 신호로도 쓴다.
set -g monitor-bell on
set -g window-status-bell-style "bg=colour3"
set -g window-status-format "#(tmux-agent-bar status #{window_index})#I #W"
set -g window-status-current-format "#(tmux-agent-bar status #{window_index})#I #W"
# left: current directory basename
set -g status-left "#[fg=colour16,bg=colour148,bold]  #I:#P #[fg=colour148,bg=colour241]` + "\ue0bc" + `#[fg=colour231,bg=colour241] #{b:pane_current_path} #[fg=colour241,bg=colour234]` + "\ue0bc" + `"
set -g status-left-length 40
# right: claude-right \uac00 ctx%+model \uc138\uadf8\uba3c\ud2b8\uc640 \ub0a0\uc9dc \uc138\uadf8\uba3c\ud2b8(bg=colour66) \uc9c4\uc785 \ud654\uc0b4\ud45c\uae4c\uc9c0 \ucd9c\ub825\ud558\ubbc0\ub85c
# \ub4a4\uc5d0\ub294 colour66 \ubc30\uacbd\uc758 \ub0a0\uc9dc \ub0b4\uc6a9\ub9cc \uc774\uc5b4\ubd99\uc778\ub2e4.
set -g status-right "#(tmux-agent-bar claude-right #{pane_id})#[fg=colour231,bg=colour66]  %m/%d  %R "
set -g status-right-length 60
`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(addition); err != nil {
		return err
	}
	fmt.Println("~/.tmux.conf: configured")
	fmt.Println("  run: tmux source-file ~/.tmux.conf")
	return nil
}

// installClaudeSettings adds missing hook entries to ~/.claude/settings.json.
// Checks per-event so newly added hooks are installed even when others already exist.
func installClaudeSettings() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".claude", "settings.json")

	// Load existing JSON (or start fresh).
	var cfg map[string]any
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parse error: %w", err)
		}
	} else {
		cfg = make(map[string]any)
	}

	// Ensure hooks map exists.
	hooks, _ := cfg["hooks"].(map[string]any)
	if hooks == nil {
		hooks = make(map[string]any)
		cfg["hooks"] = hooks
	}

	// eventHasCmd reports whether the given event already contains the command.
	// Substring match, so an absolute-path registration
	// (e.g. "/home/u/.local/bin/tmux-agent-bar hook done") counts as present.
	eventHasCmd := func(event, cmd string) bool {
		entries, _ := hooks[event].([]any)
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			cmds, _ := entry["hooks"].([]any)
			for _, c := range cmds {
				h, _ := c.(map[string]any)
				if s, ok := h["command"].(string); ok && strings.Contains(s, cmd) {
					return true
				}
			}
		}
		return false
	}

	addHook := func(event, matcher, cmd string) {
		entry := map[string]any{
			"matcher": matcher,
			"hooks": []map[string]any{
				{"type": "command", "command": cmd},
			},
		}
		existing, _ := hooks[event].([]any)
		hooks[event] = append(existing, entry)
	}

	type hookDef struct{ event, matcher, cmd string }
	wanted := []hookDef{
		{"PreToolUse", "", "tmux-agent-bar hook thinking"},
		// Plan mode: the ExitPlanMode tool is what an agent calls to present a
		// plan for approval, so flip to ⏸ when it fires. The generic PreToolUse
		// entry above runs first and sets thinking; this matched entry runs
		// after and overwrites the state file with planning.
		{"PreToolUse", "ExitPlanMode", "tmux-agent-bar hook planning"},
		{"Stop", "", "tmux-agent-bar hook done"},
		{"Notification", "", "tmux-agent-bar hook waiting"},
		{"SubagentStop", "", "tmux-agent-bar hook subagent_stop"},
		{"UserPromptSubmit", "", "tmux-agent-bar hook thinking"},
		// Clean up this pane's state/meta files the moment the session ends,
		// so an exited claude doesn't keep showing stale ctx%/state.
		{"SessionEnd", "", "tmux-agent-bar hook session_end"},
	}

	added := 0
	for _, h := range wanted {
		if !eventHasCmd(h.event, h.cmd) {
			addHook(h.event, h.matcher, h.cmd)
			added++
		}
	}

	// Route the immediate "done / needs-attention" signal to the terminal bell.
	// The status-bar emojis are an ambient indicator polled on a loose
	// status-interval, so the native bell (and tmux's monitor-bell highlight)
	// carries the instant notification instead.
	changed := false
	if cfg["preferredNotifChannel"] != "terminal_bell" {
		cfg["preferredNotifChannel"] = "terminal_bell"
		changed = true
	}

	if added == 0 && !changed {
		fmt.Println("~/.claude/settings.json: already configured, skipping")
		return nil
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Atomic temp+rename so an interrupted write can never leave a truncated
	// settings.json (which would drop every hook). writeFileAtomic creates its
	// temp file in filepath.Dir(path) (~/.claude), so the rename stays within one
	// directory and is atomic. os.CreateTemp yields 0600, so restore the
	// conventional 0644 afterward (chmod only fails if the rename already failed).
	if err := writeFileAtomic(path, out); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return err
	}
	fmt.Println("~/.claude/settings.json: configured")
	return nil
}

// aggregateWindowEmoji returns the emoji representing the collective state
// of all panes in the given window.
func aggregateWindowEmoji(session, windowIndex string) string {
	panes, err := tmuxListPanesWithCreated(session, windowIndex)
	if err != nil {
		return ""
	}

	states := make([]string, 0, len(panes))
	for _, pane := range panes {
		states = append(states, resolvePaneStateOrClear(session, windowIndex, pane))
	}
	return emojiForStates(states)
}

// resolvePaneStateOrClear returns the display state for a pane, applying
// render-time background-job resolution:
//
//   - "done" with a live claude background job displays as bg_waiting (⏳)
//     without rewriting the state file; once the job exits, the recorded
//     "done" shows ✅ again.
//   - a recorded "bg_waiting" (manual/legacy) whose job is gone is cleared
//     to idle, removing the state file.
func resolvePaneStateOrClear(session, windowIndex string, pane paneCreated) string {
	key := stateKey(session, windowIndex, pane.index)
	state := effectiveState(key, pane.created)
	switch state {
	case "done":
		if pane.pid > 0 && paneHasBackgroundJobs(pane.pid) {
			return "bg_waiting"
		}
	case "bg_waiting":
		if pane.pid > 0 && !paneHasBackgroundJobs(pane.pid) {
			os.Remove(filepath.Join(stateDir, key))
			return ""
		}
	}
	return state
}

// --- Meta (model + context usage) ---

// PaneMeta stores Claude Code session metadata for a pane.
type PaneMeta struct {
	Model       string `json:"model"`
	InputTokens int    `json:"input_tokens"` // total context tokens used
}

// hookStdin is the JSON payload Claude Code sends to hooks via stdin.
type hookStdin struct {
	TranscriptPath string `json:"transcript_path"`
	Model          string `json:"model"`
	Usage          struct {
		InputTokens              int `json:"input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// transcriptLine represents a JSONL entry from Claude Code's transcript file.
// Handles both wrapped ({message:{role,model,usage}}) and direct ({role,model,usage}) formats.
type transcriptLine struct {
	// Direct format
	Role  string `json:"role"`
	Model string `json:"model"`
	Usage struct {
		InputTokens              int `json:"input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	// Wrapped format
	Message struct {
		Role  string `json:"role"`
		Model string `json:"model"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// parseHookStdin reads and parses the JSON payload from Claude Code hook stdin.
// Returns a zero-value hookStdin and an error if stdin cannot be parsed.
// Uses a 2-second timeout to avoid hanging if the caller never closes stdin.
func parseHookStdin() (hookStdin, error) {
	type readResult struct {
		data []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(os.Stdin)
		ch <- readResult{data, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil || len(r.data) == 0 {
			return hookStdin{}, fmt.Errorf("empty stdin")
		}
		var h hookStdin
		if err := json.Unmarshal(r.data, &h); err != nil {
			return hookStdin{}, err
		}
		return h, nil
	case <-time.After(500 * time.Millisecond):
		return hookStdin{}, fmt.Errorf("stdin read timeout")
	}
}

// resolvePaneMeta extracts model and context usage from hook data.
// It first checks if the hook stdin contains the data directly; otherwise it
// reads the last assistant message from the transcript file.
func resolvePaneMeta(h hookStdin) (PaneMeta, bool) {
	// Prefer data from the transcript file (most up-to-date).
	if h.TranscriptPath != "" {
		if meta, ok := readTranscriptMeta(h.TranscriptPath); ok {
			return meta, true
		}
	}
	// Fallback: use model/usage from hook stdin directly (if provided).
	if h.Model != "" {
		total := h.Usage.InputTokens + h.Usage.CacheReadInputTokens + h.Usage.CacheCreationInputTokens
		return PaneMeta{Model: h.Model, InputTokens: total}, true
	}
	return PaneMeta{}, false
}

// readTranscriptMeta reads the last assistant message from a Claude Code JSONL
// transcript file and returns the model and total context token count.
// Uses a 1-second timeout to avoid blocking on slow/hung filesystems (e.g. NFS).
func readTranscriptMeta(path string) (PaneMeta, bool) {
	type result struct {
		meta PaneMeta
		ok   bool
	}
	ch := make(chan result, 1)
	go func() {
		m, ok := readTranscriptMetaImpl(path)
		ch <- result{m, ok}
	}()
	select {
	case r := <-ch:
		return r.meta, r.ok
	case <-time.After(500 * time.Millisecond):
		return PaneMeta{}, false
	}
}

// readTranscriptMetaImpl does the actual transcript reading.
// Reads only the last 64 KB to avoid loading large transcripts into memory.
func readTranscriptMetaImpl(path string) (PaneMeta, bool) {
	f, err := os.Open(path)
	if err != nil {
		return PaneMeta{}, false
	}
	defer f.Close()

	const readSize = 64 * 1024
	info, err := f.Stat()
	if err != nil {
		return PaneMeta{}, false
	}
	offset := info.Size() - readSize
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return PaneMeta{}, false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return PaneMeta{}, false
	}

	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var entry transcriptLine
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		// Check wrapped format first, then direct format.
		role, model := entry.Message.Role, entry.Message.Model
		u := entry.Message.Usage
		if role == "" {
			role, model = entry.Role, entry.Model
			u = entry.Usage
		}
		if role == "assistant" && model != "" {
			total := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
			return PaneMeta{Model: model, InputTokens: total}, true
		}
	}
	return PaneMeta{}, false
}

// writeMeta writes PaneMeta as JSON to the meta file for the given key.
func writeMeta(key string, m PaneMeta) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(stateDir, key+".meta"), data)
}

// readMeta reads PaneMeta from the meta file for the given key.
func readMeta(key string) (PaneMeta, bool) {
	data, err := os.ReadFile(filepath.Join(stateDir, key+".meta"))
	if err != nil {
		return PaneMeta{}, false
	}
	var m PaneMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return PaneMeta{}, false
	}
	return m, true
}

// defaultContextTokens is the assumed context-window size when
// TMUX_AGENT_BAR_CTX_LIMIT is not set.
const defaultContextTokens = 200000

// contextLimit returns the denominator for the ctx% display. Sessions with a
// larger context window (e.g. 1M) can override it by exporting
// TMUX_AGENT_BAR_CTX_LIMIT=<tokens> into the tmux server's environment.
func contextLimit() int {
	if v := os.Getenv("TMUX_AGENT_BAR_CTX_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultContextTokens
}

// shortModelName returns a short display name for a Claude model ID.
// e.g. "claude-sonnet-4-6" → "sonnet", "claude-opus-4-6" → "opus"
func shortModelName(model string) string {
	for _, tier := range []string{"opus", "sonnet", "haiku", "fable", "mythos"} {
		if strings.Contains(model, tier) {
			return tier
		}
	}
	// Fallback: strip the "claude-" prefix.
	return strings.TrimPrefix(model, "claude-")
}
