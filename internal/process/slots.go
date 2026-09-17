package process

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// On-disk layout per slot. llama.cpp writes the first two when it saves a
// slot; llama-swap writes the completion marker so a restore can tell a
// finished save from an interrupted one:
//
//	qwen38.0.slot.bin       KV state (llama.cpp)
//	qwen38.0.slot.bin.ckpt  sliding-window attention checkpoint sidecar (llama.cpp)
//	qwen38.0.slot.bin.meta  completion marker (llama-swap)
const (
	slotFileSuffix = ".slot.bin"
	slotCkptSuffix = ".ckpt"
	slotMetaSuffix = ".meta"
)

// slotResult mirrors llama-server's per-slot save/restore report.
type slotResult struct {
	NSaved    int   `json:"n_saved"`
	NRestored int   `json:"n_restored"`
	NWritten  int64 `json:"n_written"`
	NRead     int64 `json:"n_read"`
	Timings   struct {
		SaveMS    float64 `json:"save_ms"`
		RestoreMS float64 `json:"restore_ms"`
	} `json:"timings"`
}

// slotName is the base state filename for one slot (no directory). Slashes and
// colons are common in model ids and would otherwise escape the directory.
func (p *ProcessCommand) slotName(slot int) string {
	safe := strings.NewReplacer("/", "_", ":", "_", " ", "_", "..", "_").Replace(p.id)
	return fmt.Sprintf("%s.%d%s", safe, slot, slotFileSuffix)
}

// slotAction issues POST /slots/{slot}?action=... against the running upstream
// and returns the reported stats.
func (p *ProcessCommand) slotAction(ctx context.Context, slot int, action, filename string) (*slotResult, error) {
	body, err := json.Marshal(map[string]string{"filename": filename})
	if err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(p.config.Proxy, "/")
	url := fmt.Sprintf("%s/slots/%d?action=%s", base, slot, action)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d: %s", action, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var res slotResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("could not parse %s response: %w", action, err)
	}
	return &res, nil
}

// removeSlotFiles deletes the .bin, .bin.ckpt and .bin.meta for one slot.
func (p *ProcessCommand) removeSlotFiles(dir, name string) {
	for _, f := range []string{name, name + slotCkptSuffix, name + slotMetaSuffix} {
		_ = os.Remove(filepath.Join(dir, f))
	}
}

// restoreSlots loads every persisted slot from disk into a freshly started
// server. It is non-fatal: a slot that fails to load is logged and served cold,
// and its whole file set is dropped so a known-bad cache is not retried on the
// next start.
func (p *ProcessCommand) restoreSlots(ctx context.Context) error {
	sp := p.config.SlotPersistence
	var firstErr error
	for i := 0; i < sp.SlotCount(); i++ {
		name := p.slotName(i)
		meta := filepath.Join(sp.Path, name+slotMetaSuffix)
		if _, err := os.Stat(meta); err != nil {
			// No marker: either nothing was saved or the save was interrupted.
			continue
		}
		res, err := p.slotAction(ctx, i, "restore", name)
		if err != nil {
			p.removeSlotFiles(sp.Path, name)
			if firstErr == nil {
				firstErr = err
			}
			p.proxyLogger.Warnf("<%s> restore slot %d failed, serving cold: %v", p.id, i, err)
			continue
		}
		p.proxyLogger.Infof("<%s> restored slot %d: %d tokens, %d bytes", p.id, i, res.NRestored, res.NRead)
		if sp.DeleteAfterRestore {
			p.removeSlotFiles(sp.Path, name)
		}
	}
	return firstErr
}

// saveSlots writes every slot to disk while the server is still alive. The
// completion marker is removed before the save and only written after a
// confirmed save, so an interrupted save never looks complete to a later
// restore.
func (p *ProcessCommand) saveSlots(ctx context.Context) error {
	sp := p.config.SlotPersistence
	var firstErr error
	for i := 0; i < sp.SlotCount(); i++ {
		name := p.slotName(i)
		meta := filepath.Join(sp.Path, name+slotMetaSuffix)
		_ = os.Remove(meta)
		res, err := p.slotAction(ctx, i, "save", name)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			p.proxyLogger.Warnf("<%s> save slot %d failed: %v", p.id, i, err)
			continue
		}
		if res.NSaved == 0 {
			p.proxyLogger.Debugf("<%s> slot %d empty, nothing to save", p.id, i)
			continue
		}
		if err := os.WriteFile(meta, fmt.Appendf(nil, "tokens=%d bytes=%d\n", res.NSaved, res.NWritten), 0o644); err != nil {
			p.proxyLogger.Warnf("<%s> could not write marker for slot %d: %v", p.id, i, err)
			continue
		}
		p.proxyLogger.Infof("<%s> saved slot %d: %d tokens, %d bytes", p.id, i, res.NSaved, res.NWritten)
	}
	return firstErr
}
