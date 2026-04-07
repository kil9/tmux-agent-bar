package main

import (
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

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tmux-agent-bar <hook|status|claude-right|install> [args...]")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "hook":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: tmux-agent-bar hook <thinking|waiting|done|error>")
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
	case "thinking", "waiting", "done", "error", "subagent_stop", "planning":
		// valid
	default:
		fmt.Fprintf(os.Stderr, "invalid status: %s (must be thinking|waiting|done|error|subagent_stop|planning)\n", status)
		os.Exit(1)
	}

	// Read hook stdin early — stdin can only be read once.
	hookData, _ := parseHookStdin()

	// Get session_window_pane key from tmux.
	// TMUX_PANE is set when inside a tmux pane (e.g. %3).
	paneID := os.Getenv("TMUX_PANE")
	if paneID == "" {
		// Not inside tmux — silently exit.
		return
	}

	key, err := tmuxPaneKey(paneID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tmux-agent-bar hook: failed to get pane info:", err)
		os.Exit(1)
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
		parts := strings.SplitN(key, "_", 3)
		if len(parts) == 3 {
			if panes, err := tmuxListPanes(parts[0], parts[1]); err == nil {
				cleanStaleFiles(stateDir, parts[0], parts[1], panes)
			}
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
	if status == "done" {
		if planPendingExists(key) {
			os.Remove(filepath.Join(stateDir, key+".plan_pending"))
			status = "waiting"
		}
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
	parts := strings.SplitN(key, "_", 3) // session, window, pane
	if len(parts) == 3 {
		if panes, err := tmuxListPanes(parts[0], parts[1]); err == nil {
			cleanStaleFiles(stateDir, parts[0], parts[1], panes)
		}
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
		for _, suffix := range []string{".meta", ".thinking_start", ".subagent_stop", ".notify_pending", ".plan_pending"} {
			base = strings.TrimSuffix(base, suffix)
		}
		if strings.HasPrefix(base, prefix) && !alive[base] {
			os.Remove(filepath.Join(dir, name))
		}
	}
}

// runStatus prints the emoji for the given window index.
// Called from tmux window-status-format every status-interval.
func runStatus(windowIndex string) {
	session, err := tmuxCurrentSession()
	if err != nil {
		// Outside tmux or tmux unavailable — print nothing.
		return
	}

	emoji := aggregateWindowEmoji(session, windowIndex)

	// If the window shows ✅ and the user has activated (is currently viewing) it,
	// clear the done state so the check disappears.
	if emoji == "✅" {
		if activeWindow, err := tmuxCurrentWindowIndex(); err == nil && activeWindow == windowIndex {
			clearViewedStates(session, windowIndex)
			emoji = ""
		}
	}

	// For thinking state, append elapsed time in dimmed color.
	if emoji == "🧠" {
		if start, ok := thinkingStartTime(session, windowIndex); ok {
			elapsed := int(time.Since(start).Seconds())
			emoji += fmt.Sprintf("#[fg=colour8](%ds)#[fg=default]", elapsed)
		}
	}

	fmt.Print(emoji)
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
		sep    = "\ue0ba"    // powerline left-pointing solid triangle (U+E0BA)
		ctxBg  = "colour241" // context+model segment background
		dateBg = "colour66"  // date segment background (steel teal)
	)

	key, err := tmuxPaneKey(paneID)
	if err != nil {
		// Fallback: just emit the transition into the date segment.
		fmt.Printf("#[fg=%s,bg=colour234]%s", dateBg, sep)
		return
	}
	meta, ok := readMeta(key)
	if !ok || meta.Model == "" {
		// No Claude Code session active; transition directly into the date segment.
		fmt.Printf("#[fg=%s,bg=colour234]%s", dateBg, sep)
		return
	}

	const maxContextTokens = 200000
	pct := meta.InputTokens * 100 / maxContextTokens
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

// selectThinkingTime picks the start time to display from a list of thinking
// panes and the index of the most-recently-activated pane.
//
//   - 0 panes thinking → false
//   - 1 pane thinking → its mtime
//   - multiple panes thinking + lastActive is one of them → that pane's mtime
//   - multiple panes thinking + lastActive not among them → earliest mtime
func selectThinkingTime(thinking []paneTime, lastActive string) (time.Time, bool) {
	if len(thinking) == 0 {
		return time.Time{}, false
	}
	if len(thinking) == 1 {
		return thinking[0].mtime, true
	}
	for _, pt := range thinking {
		if pt.index == lastActive {
			return pt.mtime, true
		}
	}
	// Fallback: earliest mtime.
	earliest := thinking[0].mtime
	for _, pt := range thinking[1:] {
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
	return selectThinkingTime(thinking, lastActive)
}

// stateKey returns the state file name for a given session, window, and pane.
func stateKey(session, windowIndex, pane string) string {
	return session + "_" + windowIndex + "_" + pane
}

// emojiForStates returns the highest-priority emoji for the given slice of state strings.
//
// Priority: 🚨 (any error) > 💬 (any waiting) > ⏸ (any planning) > 🧠 (any thinking) > ✅ (any done) > "" (all idle)
func emojiForStates(states []string) string {
	anyError := false
	anyWaiting := false
	anyPlanning := false
	anyThinking := false
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
		return "🧠"
	case anyDone:
		return "✅"
	default:
		return ""
	}
}

// tmuxPaneKey returns "<session>_<window>_<pane>" for the given pane ID.
func tmuxPaneKey(paneID string) (string, error) {
	out, err := exec.Command("tmux", "display-message", "-p", "-t", paneID, "#S_#I_#P").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// tmuxCurrentWindowIndex returns the index of the currently active window.
func tmuxCurrentWindowIndex() (string, error) {
	out, err := exec.Command("tmux", "display-message", "-p", "#I").Output()
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
	out, err := exec.Command("tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// paneCreated pairs a pane index with its creation timestamp.
type paneCreated struct {
	index   string
	created time.Time
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
func tmuxListPanesWithCreated(session, windowIndex string) ([]paneCreated, error) {
	target := session + ":" + windowIndex
	raw, err := exec.Command("tmux", "list-panes", "-t", target, "-F", "#{pane_index} #{pane_created}").Output()
	if err != nil {
		return nil, err
	}
	var panes []paneCreated
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		pc := paneCreated{index: parts[0]}
		if len(parts) == 2 {
			if ts, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				pc.created = time.Unix(ts, 0)
			}
		}
		panes = append(panes, pc)
	}
	return panes, nil
}

// tmuxLastActivePaneIndex returns the pane_index of the most recently activated
// pane in the given session/window, based on tmux's pane_last_activity timestamp.
func tmuxLastActivePaneIndex(session, windowIndex string) (string, error) {
	target := session + ":" + windowIndex
	out, err := exec.Command("tmux", "list-panes", "-t", target, "-F", "#{pane_last_activity} #{pane_index}").Output()
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

// writePlanPending records that a plan was presented and is awaiting user approval.
func writePlanPending(key string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, key+".plan_pending"), nil, 0o644)
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
	return os.WriteFile(path, []byte(time.Now().Format(time.RFC3339Nano)), 0o644)
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
	return os.WriteFile(path, []byte(time.Now().Format(time.RFC3339Nano)), 0o644)
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
// The marker allows the subsequent Notification hook to distinguish a
// SubagentStop-triggered notification (main agent still thinking) from an
// interrupt-triggered notification (agent stopped, should show waiting).
func markSubagentStop(key string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(stateDir, key+".subagent_stop")
	return os.WriteFile(path, []byte(time.Now().Format(time.RFC3339Nano)), 0o644)
}

// recentSubagentStop reports whether a SubagentStop event occurred for the given
// key within the last 3 seconds. SubagentStop and its following Notification fire
// nearly simultaneously, so 3 s is a safe window while avoiding false positives
// for interrupt notifications that arrive later.
func recentSubagentStop(key string) bool {
	info, err := os.Stat(filepath.Join(stateDir, key+".subagent_stop"))
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < 3*time.Second
}

// writeState writes the status string to the state file for the given key.
func writeState(key, status string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(stateDir, key)
	return os.WriteFile(path, []byte(status), 0o644)
}

// readState reads the status for the given key. Returns "" if the file doesn't exist.
func readState(key string) string {
	return readStateFresh(key, time.Time{})
}

// readStateFresh reads the status for the given key, but returns "" when the
// state file's mtime predates `after` (meaning the file was written in a
// previous pane lifetime and is now stale). Pass a zero time to skip the check.
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
	return strings.TrimSpace(string(data))
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
set -g status-interval 1
set -g window-status-format "#(tmux-agent-bar status #{window_index})#I #W"
set -g window-status-current-format "#(tmux-agent-bar status #{window_index})#I #W"
# left: current directory basename; right: claude context+model + date
set -g status-left "#[fg=colour16,bg=colour148,bold]  #I:#P #[fg=colour148,bg=colour241]` + "\ue0bc" + `#[fg=colour231,bg=colour241] #{b:pane_current_path} #[fg=colour241,bg=colour234]` + "\ue0bc" + `"
set -g status-left-length 40
set -g status-right "#(tmux-agent-bar claude-right #{pane_id})#[fg=colour241,bg=colour234]` + "\ue0ba" + `#[fg=colour148,bg=colour241]  %m/%d  %R "
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
	eventHasCmd := func(event, cmd string) bool {
		entries, _ := hooks[event].([]any)
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			cmds, _ := entry["hooks"].([]any)
			for _, c := range cmds {
				h, _ := c.(map[string]any)
				if h["command"] == cmd {
					return true
				}
			}
		}
		return false
	}

	addHook := func(event, cmd string) {
		entry := map[string]any{
			"matcher": "",
			"hooks": []map[string]any{
				{"type": "command", "command": cmd},
			},
		}
		existing, _ := hooks[event].([]any)
		hooks[event] = append(existing, entry)
	}

	type hookDef struct{ event, cmd string }
	wanted := []hookDef{
		{"PreToolUse", "tmux-agent-bar hook thinking"},
		{"Stop", "tmux-agent-bar hook done"},
		{"Notification", "tmux-agent-bar hook waiting"},
		{"SubagentStop", "tmux-agent-bar hook subagent_stop"},
		{"EnterPlanMode", "tmux-agent-bar hook planning"},
		{"ExitPlanMode", "tmux-agent-bar hook thinking"},
	}

	added := 0
	for _, h := range wanted {
		if !eventHasCmd(h.event, h.cmd) {
			addHook(h.event, h.cmd)
			added++
		}
	}

	if added == 0 {
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
	if err := os.WriteFile(path, out, 0o644); err != nil {
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
		key := stateKey(session, windowIndex, pane.index)
		states = append(states, effectiveState(key, pane.created))
	}
	return emojiForStates(states)
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
func parseHookStdin() (hookStdin, error) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil || len(data) == 0 {
		return hookStdin{}, fmt.Errorf("empty stdin")
	}
	var h hookStdin
	if err := json.Unmarshal(data, &h); err != nil {
		return hookStdin{}, err
	}
	return h, nil
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
// Reads only the last 64 KB to avoid loading large transcripts into memory.
func readTranscriptMeta(path string) (PaneMeta, bool) {
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
	return os.WriteFile(filepath.Join(stateDir, key+".meta"), data, 0o644)
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

// shortModelName returns a short display name for a Claude model ID.
// e.g. "claude-sonnet-4-6" → "sonnet", "claude-opus-4-6" → "opus"
func shortModelName(model string) string {
	for _, tier := range []string{"opus", "sonnet", "haiku"} {
		if strings.Contains(model, tier) {
			return tier
		}
	}
	// Fallback: strip the "claude-" prefix.
	return strings.TrimPrefix(model, "claude-")
}
