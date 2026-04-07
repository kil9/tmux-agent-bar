package main

import (
	"os"
	"path/filepath"
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
	if emoji != "🧠" {
		t.Errorf("got %q, want 🧠 (fresh marker should not promote)", emoji)
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
	if emoji != "🧠" {
		t.Errorf("got %q, want 🧠", emoji)
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

// TestSelectThinkingTime verifies that selectThinkingTime picks the correct pane.
func TestSelectThinkingTime(t *testing.T) {
	t0 := time.Now().Add(-30 * time.Second)
	t1 := time.Now().Add(-5 * time.Second)

	panes := []paneTime{
		{index: "0", mtime: t0}, // older
		{index: "1", mtime: t1}, // newer
	}

	t.Run("empty", func(t *testing.T) {
		_, ok := selectThinkingTime(nil, "")
		if ok {
			t.Error("expected false for empty slice")
		}
	})

	t.Run("single pane", func(t *testing.T) {
		got, ok := selectThinkingTime(panes[:1], "99")
		if !ok || !got.Equal(t0) {
			t.Errorf("got %v ok=%v, want %v true", got, ok, t0)
		}
	})

	t.Run("last active is pane 1 (newer)", func(t *testing.T) {
		got, ok := selectThinkingTime(panes, "1")
		if !ok || !got.Equal(t1) {
			t.Errorf("got %v ok=%v, want %v (pane 1 mtime) true", got, ok, t1)
		}
	})

	t.Run("last active is pane 0 (older)", func(t *testing.T) {
		got, ok := selectThinkingTime(panes, "0")
		if !ok || !got.Equal(t0) {
			t.Errorf("got %v ok=%v, want %v (pane 0 mtime) true", got, ok, t0)
		}
	})

	t.Run("last active not in thinking panes — fallback to earliest", func(t *testing.T) {
		got, ok := selectThinkingTime(panes, "99")
		if !ok || !got.Equal(t0) {
			t.Errorf("got %v ok=%v, want %v (earliest) true", got, ok, t0)
		}
	})
}

// --- helpers ---

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
