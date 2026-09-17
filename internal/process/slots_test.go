package process

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// callLog records which slot actions the fake upstream saw, in order.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, name)
}

func (l *callLog) has(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.calls {
		if c == name {
			return true
		}
	}
	return false
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// slotTestUpstream is a stand-in llama-server: always healthy, and it records
// save/restore actions on /slots/{id}?action=... .
func slotTestUpstream(t *testing.T) (*httptest.Server, *callLog) {
	t.Helper()
	log := &callLog{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch action := r.URL.Query().Get("action"); action {
		case "restore":
			log.add("restore")
			fmt.Fprint(w, `{"n_restored":128,"n_read":4096}`)
		case "save":
			log.add("save")
			fmt.Fprint(w, `{"n_saved":128,"n_written":4096}`)
		default:
			// /health and anything else: 200 so the health check passes.
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)
	return server, log
}

// TestProcessCommand_SlotRestoreOnStart_SaveOnStop is the core guarantee: a
// restore runs inside doStart (so it precedes StateReady and the first request
// can never race it), and a save runs on Stop before the upstream is signalled.
func TestProcessCommand_SlotRestoreOnStart_SaveOnStop(t *testing.T) {
	slotsDir := t.TempDir()
	ts, log := slotTestUpstream(t)

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                "sleep 30",
		Proxy:              ts.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		SlotPersistence:    &config.SlotPersistenceConfig{Path: slotsDir},
	})
	t.Cleanup(func() { p.Stop(testStopTimeout) }) //nolint: errcheck

	name := p.slotName(0)
	// Pre-seed a completed save so the restore has a marker to act on.
	if err := os.WriteFile(filepath.Join(slotsDir, name+slotMetaSuffix), []byte("tokens=128\n"), 0o644); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	if err := p.EnsureReady(context.Background(), testStartTimeout); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if !log.has("restore") {
		t.Fatalf("expected a restore before ready, calls=%v", log.all())
	}

	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !log.has("save") {
		t.Fatalf("expected a save on stop, calls=%v", log.all())
	}
}

// TestProcessCommand_SlotDeleteAfterRestore verifies that with
// DeleteAfterRestore the whole file set (.bin, .bin.ckpt, .bin.meta) is removed
// after a successful restore.
func TestProcessCommand_SlotDeleteAfterRestore(t *testing.T) {
	slotsDir := t.TempDir()
	ts, log := slotTestUpstream(t)

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                "sleep 30",
		Proxy:              ts.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		SlotPersistence:    &config.SlotPersistenceConfig{Path: slotsDir, DeleteAfterRestore: true},
	})
	t.Cleanup(func() { p.Stop(testStopTimeout) }) //nolint: errcheck

	name := p.slotName(0)
	bin := filepath.Join(slotsDir, name) // slotName already ends in .slot.bin
	ckpt := bin + slotCkptSuffix
	meta := bin + slotMetaSuffix
	for _, f := range []string{bin, ckpt, meta} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", f, err)
		}
	}

	if err := p.EnsureReady(context.Background(), testStartTimeout); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if !log.has("restore") {
		t.Fatalf("expected a restore, calls=%v", log.all())
	}
	for _, f := range []string{bin, ckpt, meta} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("expected %s removed after restore, still present", f)
		}
	}
}
