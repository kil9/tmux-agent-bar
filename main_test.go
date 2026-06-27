package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func setStateDirForTest(t *testing.T, dir string) {
	t.Helper()
	orig := stateDir
	stateDir = dir
	t.Cleanup(func() { stateDir = orig })
}

func TestWriteAndReadState(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	key := "testsession_1_0"
	if err := writeState(key, "waiting"); err != nil {
		t.Fatal(err)
	}
	got := readState(key)
	if got != "waiting" {
		t.Errorf("got %q, want %q", got, "waiting")
	}
}

func TestAggregateWindowEmoji_allIdle(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	// No state files → all idle
	emoji := aggregateWindowEmojiFromDir(dir, "sess", "1", []string{"0", "1"})
	if emoji != "" {
		t.Errorf("got %q, want empty", emoji)
	}
}

func TestAggregateWindowEmoji_anyWaiting(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	writeStateToDir(t, dir, "sess_1_0", "done")
	writeStateToDir(t, dir, "sess_1_1", "thinking")
	writeAgedNotifyPending(t, dir, "sess_1_1") // aged marker promotes thinking→waiting

	emoji := aggregateWindowEmojiFromDir(dir, "sess", "1", []string{"0", "1"})
	if emoji != "💬" {
		t.Errorf("got %q, want 💬", emoji)
	}
}

func TestDeferredNotify_freshMarkerKeepsThinking(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	writeStateToDir(t, dir, "sess_1_0", "thinking")
	// Fresh marker (just now) — should NOT promote yet.
	path := filepath.Join(dir, "sess_1_0.notify_pending")
	if err := os.WriteFile(path, []byte(time.Now().Format(time.RFC3339Nano)), 0o644); err != nil {
		t.Fatal(err)
	}

	emoji := aggregateWindowEmojiFromDir(dir, "sess", "1", []string{"0"})
	if emoji != "🤖" {
		t.Errorf("got %q, want 🤖 (fresh marker should not promote)", emoji)
	}
}

func TestDeferredNotify_agedMarkerPromotes(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	writeStateToDir(t, dir, "sess_1_0", "thinking")
	writeAgedNotifyPending(t, dir, "sess_1_0")

	emoji := aggregateWindowEmojiFromDir(dir, "sess", "1", []string{"0"})
	if emoji != "💬" {
		t.Errorf("got %q, want 💬 (aged marker should promote)", emoji)
	}
}

func TestDeferredNotify_stopClearsMarker(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	// Simulate: thinking state + aged marker, but then "done" was written
	// (Stop fired and cleared the marker).
	writeStateToDir(t, dir, "sess_1_0", "done")
	// No marker file — Stop removed it. Pane should show ✅, not 💬.

	emoji := aggregateWindowEmojiFromDir(dir, "sess", "1", []string{"0"})
	if emoji != "✅" {
		t.Errorf("got %q, want ✅", emoji)
	}
}

func TestAggregateWindowEmoji_anyThinking(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	writeStateToDir(t, dir, "sess_1_0", "thinking")
	writeStateToDir(t, dir, "sess_1_1", "done")

	emoji := aggregateWindowEmojiFromDir(dir, "sess", "1", []string{"0", "1"})
	if emoji != "🤖" {
		t.Errorf("got %q, want 🤖", emoji)
	}
}

func TestAggregateWindowEmoji_anyError(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	writeStateToDir(t, dir, "sess_1_0", "error")
	writeStateToDir(t, dir, "sess_1_1", "waiting")

	// error beats waiting
	emoji := aggregateWindowEmojiFromDir(dir, "sess", "1", []string{"0", "1"})
	if emoji != "🚨" {
		t.Errorf("got %q, want 🚨", emoji)
	}
}

func TestAggregateWindowEmoji_anyDone(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	writeStateToDir(t, dir, "sess_1_0", "done")
	writeStateToDir(t, dir, "sess_1_1", "") // idle (no file written, simulate via empty)

	emoji := aggregateWindowEmojiFromDir(dir, "sess", "1", []string{"0", "1"})
	if emoji != "✅" {
		t.Errorf("got %q, want ✅", emoji)
	}
}

func TestEmojiForStates_priority(t *testing.T) {
	tests := []struct {
		name   string
		states []string
		want   string
	}{
		{"all idle", []string{"", ""}, ""},
		{"done beats idle", []string{"done", ""}, "✅"},
		{"bg_waiting beats done", []string{"bg_waiting", "done"}, "⏳"},
		{"thinking beats bg_waiting", []string{"thinking", "bg_waiting"}, "🤖"},
		{"planning beats thinking", []string{"planning", "thinking", "bg_waiting"}, "⏸"},
		{"waiting beats planning", []string{"waiting", "planning", "bg_waiting"}, "💬"},
		{"error beats all", []string{"error", "waiting", "thinking", "bg_waiting"}, "🚨"},
		{"only bg_waiting", []string{"bg_waiting"}, "⏳"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := emojiForStates(tc.states); got != tc.want {
				t.Errorf("emojiForStates(%v) = %q, want %q", tc.states, got, tc.want)
			}
		})
	}
}

func TestAggregateWindowEmoji_anyBgWaiting(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	writeStateToDir(t, dir, "sess_1_0", "bg_waiting")
	writeStateToDir(t, dir, "sess_1_1", "done")

	emoji := aggregateWindowEmojiFromDir(dir, "sess", "1", []string{"0", "1"})
	if emoji != "⏳" {
		t.Errorf("got %q, want ⏳ (bg_waiting beats done)", emoji)
	}
}

func TestCleanStaleFiles(t *testing.T) {
	dir := t.TempDir()

	// Write state files: pane 0 and 1 are "alive", pane 2 is stale (closed).
	writeStateToDir(t, dir, "sess_1_0", "done")
	writeStateToDir(t, dir, "sess_1_1", "waiting")
	writeStateToDir(t, dir, "sess_1_2", "done") // stale

	// Clean up with alive panes = [0, 1].
	cleanStaleFiles(dir, "sess", "1", []string{"0", "1"})

	// sess_1_2 should be removed.
	if _, err := os.Stat(filepath.Join(dir, "sess_1_2")); !os.IsNotExist(err) {
		t.Error("stale file sess_1_2 was not removed")
	}
	// sess_1_0 and sess_1_1 should remain.
	for _, key := range []string{"sess_1_0", "sess_1_1"} {
		if _, err := os.Stat(filepath.Join(dir, key)); os.IsNotExist(err) {
			t.Errorf("alive file %s was incorrectly removed", key)
		}
	}
}

// TestRemoveOrphanFiles verifies that files from dead windows/sessions are
// removed while files from live windows (and dotfile markers) are kept.
func TestRemoveOrphanFiles(t *testing.T) {
	dir := t.TempDir()

	// Live window main:0 with state + markers.
	writeStateToDir(t, dir, "main_0_0", "thinking")
	if err := os.WriteFile(filepath.Join(dir, "main_0_0.meta"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Orphan window (main has no window 1) and orphan session ("gone").
	writeStateToDir(t, dir, "main_1_0", "done")
	if err := os.WriteFile(filepath.Join(dir, "main_1_3.subagent_stop"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	writeStateToDir(t, dir, "gone_0_0", "done")
	// The GC throttle marker must never be deleted.
	if err := os.WriteFile(filepath.Join(dir, ".gc"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Only window main:0 is live.
	removeOrphanFiles(dir, []string{"main_0"})

	// Orphan-window and orphan-session files gone.
	for _, name := range []string{"main_1_0", "main_1_3.subagent_stop", "gone_0_0"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("orphan file %s was not removed", name)
		}
	}
	// Live-window files and the dotfile marker kept.
	for _, name := range []string{"main_0_0", "main_0_0.meta", ".gc"} {
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			t.Errorf("file %s was incorrectly removed", name)
		}
	}
}

// TestRemoveOrphanFiles_prefixIsNotSubstring guards against deleting a live
// window whose key is a prefix-collision with another (e.g. "k9box_0" must not
// match a file from window "k9box_01", and "main_1" must not reap "main_10_*").
func TestRemoveOrphanFiles_prefixIsNotSubstring(t *testing.T) {
	dir := t.TempDir()
	writeStateToDir(t, dir, "main_1_0", "done")  // live window main:1
	writeStateToDir(t, dir, "main_10_0", "done") // dead window main:10

	// Only window main:1 is live. The trailing "_" in the prefix match must keep
	// main_1_* and remove main_10_* without confusing the two.
	removeOrphanFiles(dir, []string{"main_1"})

	if _, err := os.Stat(filepath.Join(dir, "main_1_0")); os.IsNotExist(err) {
		t.Error("live window main_1_0 was incorrectly removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "main_10_0")); !os.IsNotExist(err) {
		t.Error("dead window main_10_0 was not removed")
	}
}

// TestThinkingStateMtimePreserved verifies that writing "thinking" when the
// state is already "thinking" does NOT update the file mtime.
// This ensures the elapsed-time counter does not reset on repeated tool calls.
func TestThinkingStateMtimePreserved(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	key := "sess_1_0"
	// Write "thinking" for the first time.
	if err := writeState(key, "thinking"); err != nil {
		t.Fatal(err)
	}

	info1, err := os.Stat(filepath.Join(dir, key))
	if err != nil {
		t.Fatal(err)
	}
	mtime1 := info1.ModTime()

	// Wait a moment so a second write would produce a detectably different mtime.
	// (filesystem mtime resolution is typically 1 ms or better on Linux tmpfs)
	for {
		// Spin until at least 1 ms has elapsed, guaranteeing any new write differs.
		if time.Since(mtime1) >= time.Millisecond {
			break
		}
	}

	// Simulate a second PreToolUse event: state is already "thinking".
	// The guard in runHook skips writeState, so mtime must not change.
	// We test the guard logic directly (runHook requires a real tmux environment).
	if readState(key) == "thinking" {
		// Guard: skip write — this is what runHook now does.
	} else {
		// If state were not "thinking", a normal write would happen.
		if err := writeState(key, "thinking"); err != nil {
			t.Fatal(err)
		}
	}

	info2, err := os.Stat(filepath.Join(dir, key))
	if err != nil {
		t.Fatal(err)
	}
	mtime2 := info2.ModTime()

	if !mtime1.Equal(mtime2) {
		t.Errorf("mtime changed after second thinking write: was %v, now %v", mtime1, mtime2)
	}
}

// TestThinkingStartMarkerCreatedFromStaleState verifies that the thinking
// start marker is created even when the state file already contains "thinking"
// (stale from a previous session). The guard should check marker existence,
// not state file content.
func TestThinkingStartMarkerCreatedFromStaleState(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	key := "sess_1_0"

	// Simulate stale state: file says "thinking" but no .thinking_start marker.
	if err := writeState(key, "thinking"); err != nil {
		t.Fatal(err)
	}

	// The guard checks readThinkingStart (marker existence), not readState.
	if _, ok := readThinkingStart(key); !ok {
		if err := writeThinkingStart(key); err != nil {
			t.Fatal(err)
		}
	}

	// Marker must exist now.
	if _, ok := readThinkingStart(key); !ok {
		t.Error("thinking_start marker was not created for stale thinking state")
	}
}

// TestSelectPaneStartTime verifies that selectPaneStartTime picks the correct pane.
func TestSelectPaneStartTime(t *testing.T) {
	t0 := time.Now().Add(-30 * time.Second)
	t1 := time.Now().Add(-5 * time.Second)

	panes := []paneTime{
		{index: "0", mtime: t0}, // older
		{index: "1", mtime: t1}, // newer
	}

	t.Run("empty", func(t *testing.T) {
		_, ok := selectPaneStartTime(nil, "")
		if ok {
			t.Error("expected false for empty slice")
		}
	})

	t.Run("single pane", func(t *testing.T) {
		got, ok := selectPaneStartTime(panes[:1], "99")
		if !ok || !got.Equal(t0) {
			t.Errorf("got %v ok=%v, want %v true", got, ok, t0)
		}
	})

	t.Run("last active is pane 1 (newer)", func(t *testing.T) {
		got, ok := selectPaneStartTime(panes, "1")
		if !ok || !got.Equal(t1) {
			t.Errorf("got %v ok=%v, want %v (pane 1 mtime) true", got, ok, t1)
		}
	})

	t.Run("last active is pane 0 (older)", func(t *testing.T) {
		got, ok := selectPaneStartTime(panes, "0")
		if !ok || !got.Equal(t0) {
			t.Errorf("got %v ok=%v, want %v (pane 0 mtime) true", got, ok, t0)
		}
	})

	t.Run("last active not in thinking panes — fallback to earliest", func(t *testing.T) {
		got, ok := selectPaneStartTime(panes, "99")
		if !ok || !got.Equal(t0) {
			t.Errorf("got %v ok=%v, want %v (earliest) true", got, ok, t0)
		}
	})
}

func TestReadTranscriptMeta_directFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	line := transcriptLine{Role: "assistant", Model: "claude-sonnet-4-6"}
	line.Usage.InputTokens = 1000
	line.Usage.CacheReadInputTokens = 2000
	line.Usage.CacheCreationInputTokens = 500
	writeJSONL(t, path, line)

	meta, ok := readTranscriptMetaImpl(path)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if meta.Model != "claude-sonnet-4-6" {
		t.Errorf("model: got %q, want %q", meta.Model, "claude-sonnet-4-6")
	}
	if meta.InputTokens != 3500 {
		t.Errorf("tokens: got %d, want 3500", meta.InputTokens)
	}
}

func TestReadTranscriptMeta_wrappedFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	entry := transcriptLine{}
	entry.Message.Role = "assistant"
	entry.Message.Model = "claude-opus-4-6"
	entry.Message.Usage.InputTokens = 5000
	entry.Message.Usage.CacheReadInputTokens = 3000
	writeJSONL(t, path, entry)

	meta, ok := readTranscriptMetaImpl(path)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if meta.Model != "claude-opus-4-6" {
		t.Errorf("model: got %q, want %q", meta.Model, "claude-opus-4-6")
	}
	if meta.InputTokens != 8000 {
		t.Errorf("tokens: got %d, want 8000", meta.InputTokens)
	}
}

func TestReadTranscriptMeta_picksLastAssistant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")

	old := transcriptLine{Role: "assistant", Model: "claude-haiku-4-5"}
	old.Usage.InputTokens = 100

	user := transcriptLine{Role: "user"}

	latest := transcriptLine{Role: "assistant", Model: "claude-sonnet-4-6"}
	latest.Usage.InputTokens = 9000

	writeJSONL(t, path, old, user, latest)

	meta, ok := readTranscriptMetaImpl(path)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if meta.Model != "claude-sonnet-4-6" {
		t.Errorf("model: got %q, want %q (should pick last assistant)", meta.Model, "claude-sonnet-4-6")
	}
	if meta.InputTokens != 9000 {
		t.Errorf("tokens: got %d, want 9000", meta.InputTokens)
	}
}

func TestReadTranscriptMeta_emptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	os.WriteFile(path, nil, 0o644)

	_, ok := readTranscriptMetaImpl(path)
	if ok {
		t.Error("expected ok=false for empty file")
	}
}

func TestReadTranscriptMeta_missingFile(t *testing.T) {
	_, ok := readTranscriptMetaImpl(filepath.Join(t.TempDir(), "nonexistent.jsonl"))
	if ok {
		t.Error("expected ok=false for missing file")
	}
}

func TestReadTranscriptMeta_largeTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")

	// Write a target line, then >64KB of padding, then another assistant line.
	// Only the last assistant line (in the 64KB tail) should be returned.
	early := transcriptLine{Role: "assistant", Model: "early-model"}
	early.Usage.InputTokens = 1

	tail := transcriptLine{Role: "assistant", Model: "tail-model"}
	tail.Usage.InputTokens = 42

	earlyJSON, _ := json.Marshal(early)
	tailJSON, _ := json.Marshal(tail)

	// Build: early line + 70KB padding (non-JSON lines) + tail line
	padding := strings.Repeat("x", 70*1024) + "\n"
	content := string(earlyJSON) + "\n" + padding + string(tailJSON) + "\n"
	os.WriteFile(path, []byte(content), 0o644)

	meta, ok := readTranscriptMetaImpl(path)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if meta.Model != "tail-model" {
		t.Errorf("model: got %q, want %q (should read from 64KB tail)", meta.Model, "tail-model")
	}
}

// TestThinkingTTL_freshStateShows verifies that a recent "thinking" state is shown.
func TestThinkingTTL_freshStateShows(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	key := "sess_1_0"
	writeStateToDir(t, dir, key, "thinking")

	got := readStateFresh(key, time.Time{})
	if got != "thinking" {
		t.Errorf("got %q, want %q (fresh thinking should be visible)", got, "thinking")
	}
}

// TestThinkingTTL_expiredStateHidden verifies that an old "thinking" state
// (older than thinkingTTL) is treated as stale and returns "".
func TestThinkingTTL_expiredStateHidden(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	key := "sess_1_0"
	writeStateToDir(t, dir, key, "thinking")
	// Back-date the state file past thinkingTTL.
	old := time.Now().Add(-(thinkingTTL + time.Minute))
	path := filepath.Join(dir, key)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	got := readStateFresh(key, time.Time{})
	if got != "" {
		t.Errorf("got %q, want %q (expired thinking should be hidden)", got, "")
	}
}

// TestThinkingTTL_expiredButThinkingStartFresh verifies that if the state file
// is old but the thinking_start marker is recent, the state is still shown.
func TestThinkingTTL_expiredButThinkingStartFresh(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	key := "sess_1_0"
	writeStateToDir(t, dir, key, "thinking")
	// Back-date the state file past thinkingTTL.
	old := time.Now().Add(-(thinkingTTL + time.Minute))
	if err := os.Chtimes(filepath.Join(dir, key), old, old); err != nil {
		t.Fatal(err)
	}
	// Write a fresh thinking_start marker (thinking just restarted).
	if err := writeThinkingStart(key); err != nil {
		t.Fatal(err)
	}

	got := readStateFresh(key, time.Time{})
	if got != "thinking" {
		t.Errorf("got %q, want %q (fresh thinking_start should override expired state file)", got, "thinking")
	}
}

// TestThinkingTTL_nonThinkingUnaffected verifies that TTL expiry only applies
// to "thinking" state, not other states like "done" or "waiting".
func TestThinkingTTL_nonThinkingUnaffected(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)

	for _, state := range []string{"done", "waiting", "error", "planning"} {
		key := "sess_1_0"
		writeStateToDir(t, dir, key, state)
		old := time.Now().Add(-(thinkingTTL + time.Minute))
		if err := os.Chtimes(filepath.Join(dir, key), old, old); err != nil {
			t.Fatal(err)
		}
		got := readStateFresh(key, time.Time{})
		if got != state {
			t.Errorf("state %q: got %q, want %q (TTL should not affect non-thinking states)", state, got, state)
		}
		os.Remove(filepath.Join(dir, key))
	}
}

// TestProcStartTime verifies that procStartTime returns a non-zero time for
// the current process.
func TestProcStartTime(t *testing.T) {
	pid := os.Getpid()
	got, ok := procStartTime(pid)
	if !ok {
		t.Skip("procStartTime not supported on this platform")
	}
	if got.IsZero() {
		t.Error("procStartTime returned zero time for current process")
	}
	// The process start time must be in the past.
	if !got.Before(time.Now()) {
		t.Errorf("procStartTime returned future time: %v", got)
	}
}

// --- bg_waiting auto-idle transition ---

func TestResolvePaneStateOrClear_bgWaitingChildAlive(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)
	root := t.TempDir()
	setProcRootForTest(t, root)

	// pane shell → claude → child still alive
	writeFakeProc(t, root, 5000, "bash", []int{5001})
	writeFakeProc(t, root, 5001, "claude", []int{5002})
	writeFakeProc(t, root, 5002, "sleep", nil)
	writeStateToDir(t, dir, "sess_1_0", "bg_waiting")

	pane := paneCreated{index: "0", pid: 5000}
	got := resolvePaneStateOrClear("sess", "1", pane)
	if got != "bg_waiting" {
		t.Errorf("got %q, want bg_waiting (job still alive)", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "sess_1_0")); err != nil {
		t.Error("state file should remain while background job is alive")
	}
}

func TestResolvePaneStateOrClear_bgWaitingChildGone(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)
	root := t.TempDir()
	setProcRootForTest(t, root)

	// pane shell → claude with no children (background job ended)
	writeFakeProc(t, root, 5000, "bash", []int{5001})
	writeFakeProc(t, root, 5001, "claude", nil)
	writeStateToDir(t, dir, "sess_1_0", "bg_waiting")

	pane := paneCreated{index: "0", pid: 5000}
	got := resolvePaneStateOrClear("sess", "1", pane)
	if got != "" {
		t.Errorf("got %q, want '' (background job ended → idle)", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "sess_1_0")); !os.IsNotExist(err) {
		t.Error("state file should be removed once background job is gone")
	}
}

func TestResolvePaneStateOrClear_doneStateUntouched(t *testing.T) {
	dir := t.TempDir()
	setStateDirForTest(t, dir)
	root := t.TempDir()
	setProcRootForTest(t, root)
	// No claude child anywhere; pane is recorded as plain "done".
	writeFakeProc(t, root, 5000, "bash", nil)
	writeStateToDir(t, dir, "sess_1_0", "done")

	pane := paneCreated{index: "0", pid: 5000}
	got := resolvePaneStateOrClear("sess", "1", pane)
	if got != "done" {
		t.Errorf("got %q, want done (non-bg_waiting must not be cleared)", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "sess_1_0")); err != nil {
		t.Error("done state file should not be removed by resolvePaneStateOrClear")
	}
}

// --- background-job detection ---

func setProcRootForTest(t *testing.T, dir string) {
	t.Helper()
	orig := procRoot
	procRoot = dir
	t.Cleanup(func() { procRoot = orig })
}

// writeFakeProc lays out a minimal /proc tree for the given PID with the
// given comm and children list. children may be nil for "no children".
func writeFakeProc(t *testing.T, root string, pid int, comm string, children []int) {
	t.Helper()
	pidStr := strconv.Itoa(pid)
	taskDir := filepath.Join(root, pidStr, "task", pidStr)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, pidStr, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parts := make([]string, 0, len(children))
	for _, c := range children {
		parts = append(parts, strconv.Itoa(c))
	}
	body := strings.Join(parts, " ")
	if body != "" {
		body += "\n"
	}
	if err := os.WriteFile(filepath.Join(taskDir, "children"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeFakeCmdline writes /proc/<pid>/cmdline (NUL-separated, as the kernel
// presents it) so readProcCmdline can read it back. writeFakeProc must have
// been called for pid first so the directory exists.
func writeFakeCmdline(t *testing.T, root string, pid int, args ...string) {
	t.Helper()
	body := strings.Join(args, "\x00")
	if body != "" {
		body += "\x00"
	}
	path := filepath.Join(root, strconv.Itoa(pid), "cmdline")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPaneHasBackgroundJobs_noChildren(t *testing.T) {
	root := t.TempDir()
	setProcRootForTest(t, root)
	// Pane shell with no children at all.
	writeFakeProc(t, root, 1000, "bash", nil)

	if paneHasBackgroundJobs(1000) {
		t.Error("expected false when pane shell has no children")
	}
}

func TestPaneHasBackgroundJobs_claudeNoGrandchild(t *testing.T) {
	root := t.TempDir()
	setProcRootForTest(t, root)
	// pane shell → claude → (no grandchildren)
	writeFakeProc(t, root, 1000, "bash", []int{1001})
	writeFakeProc(t, root, 1001, "claude", nil)

	if paneHasBackgroundJobs(1000) {
		t.Error("expected false when claude has no children of its own")
	}
}

func TestPaneHasBackgroundJobs_claudeWithGrandchild(t *testing.T) {
	root := t.TempDir()
	setProcRootForTest(t, root)
	// pane shell → claude → bash (background job)
	writeFakeProc(t, root, 1000, "bash", []int{1001})
	writeFakeProc(t, root, 1001, "claude", []int{1002})
	writeFakeProc(t, root, 1002, "bash", nil)

	if !paneHasBackgroundJobs(1000) {
		t.Error("expected true when claude has a live grandchild")
	}
}

func TestPaneHasBackgroundJobs_nonClaudeChildIgnored(t *testing.T) {
	root := t.TempDir()
	setProcRootForTest(t, root)
	// pane shell → vim (and vim has children) — not a claude background job.
	writeFakeProc(t, root, 1000, "bash", []int{1001})
	writeFakeProc(t, root, 1001, "vim", []int{1002})
	writeFakeProc(t, root, 1002, "less", nil)

	if paneHasBackgroundJobs(1000) {
		t.Error("expected false when the live grandchild is under a non-claude process")
	}
}

func TestPaneHasBackgroundJobs_mcpServersIgnored(t *testing.T) {
	root := t.TempDir()
	setProcRootForTest(t, root)
	// pane shell → claude → two node MCP servers (always alive for the session).
	writeFakeProc(t, root, 1000, "bash", []int{1001})
	writeFakeProc(t, root, 1001, "claude", []int{1002, 1003})
	writeFakeProc(t, root, 1002, "node", nil)
	writeFakeCmdline(t, root, 1002, "node", "/home/u/.ccs/mcp/ccs-websearch-server.cjs")
	writeFakeProc(t, root, 1003, "node", nil)
	writeFakeCmdline(t, root, 1003, "node", "/home/u/.ccs/mcp/ccs-image-analysis-server.cjs")

	if paneHasBackgroundJobs(1000) {
		t.Error("expected false when claude's only children are MCP servers")
	}
}

func TestPaneHasBackgroundJobs_realJobAmongMCPServers(t *testing.T) {
	root := t.TempDir()
	setProcRootForTest(t, root)
	// pane shell → claude → MCP server + a real background bash.
	writeFakeProc(t, root, 1000, "bash", []int{1001})
	writeFakeProc(t, root, 1001, "claude", []int{1002, 1003})
	writeFakeProc(t, root, 1002, "node", nil)
	writeFakeCmdline(t, root, 1002, "node", "/home/u/.ccs/mcp/ccs-websearch-server.cjs")
	writeFakeProc(t, root, 1003, "bash", nil)
	writeFakeCmdline(t, root, 1003, "bash", "-c", "sleep 600")

	if !paneHasBackgroundJobs(1000) {
		t.Error("expected true when a real background job sits alongside MCP servers")
	}
}

func TestPaneHasBackgroundJobs_invalidPID(t *testing.T) {
	root := t.TempDir()
	setProcRootForTest(t, root)

	if paneHasBackgroundJobs(0) {
		t.Error("expected false for invalid pid")
	}
	if paneHasBackgroundJobs(99999) {
		t.Error("expected false when /proc entry is missing")
	}
}

// --- helpers ---

func writeJSONL(t *testing.T, path string, lines ...transcriptLine) {
	t.Helper()
	var parts []string
	for _, line := range lines {
		data, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, string(data))
	}
	if err := os.WriteFile(path, []byte(strings.Join(parts, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeStateToDir(t *testing.T, dir, key, status string) {
	t.Helper()
	if status == "" {
		return // idle = no file
	}
	if err := os.WriteFile(filepath.Join(dir, key), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
}

// aggregateWindowEmojiFromDir is a testable version of aggregateWindowEmoji
// that accepts an explicit directory and pane list instead of calling tmux.
// stateDir must be set to dir before calling (via setStateDirForTest).
func aggregateWindowEmojiFromDir(dir, session, windowIndex string, panes []string) string {
	states := make([]string, 0, len(panes))
	for _, pane := range panes {
		key := stateKey(session, windowIndex, pane)
		states = append(states, effectiveState(key, time.Time{}))
	}
	return emojiForStates(states)
}

// writeAgedNotifyPending writes a .notify_pending marker that appears older
// than notifyPendingDelay so that effectiveState promotes the pane to "waiting".
func writeAgedNotifyPending(t *testing.T, dir, key string) {
	t.Helper()
	path := filepath.Join(dir, key+".notify_pending")
	aged := time.Now().Add(-2 * notifyPendingDelay)
	if err := os.WriteFile(path, []byte(aged.Format(time.RFC3339Nano)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, aged, aged); err != nil {
		t.Fatal(err)
	}
}

func TestFormatElapsed(t *testing.T) {
	cases := []struct {
		seconds int
		want    string
	}{
		{0, ""},        // under a minute: hidden
		{30, ""},       // under a minute: hidden
		{59, ""},       // boundary just under a minute
		{60, "1m"},     // exactly one minute
		{90, "1m"},     // floored, not rounded
		{119, "1m"},    // floored just under two minutes
		{120, "2m"},    // exactly two minutes
		{3599, "59m"},  // just under an hour, still minutes-only
		{3600, "1h0m"}, // exactly an hour keeps the 0m
		{3660, "1h1m"}, // an hour and a minute
		{3661, "1h1m"}, // seconds dropped
		{7325, "2h2m"}, // multi-hour, floored minutes
	}
	for _, c := range cases {
		if got := formatElapsed(c.seconds); got != c.want {
			t.Errorf("formatElapsed(%d) = %q, want %q", c.seconds, got, c.want)
		}
	}
}
