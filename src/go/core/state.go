package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// writeFileAtomic writes via a temp file + rename, so a crash mid-write never
// leaves a truncated file.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadState reads state.json; a missing or damaged file means no state yet.
func loadState() State {
	st := State{}
	data, err := os.ReadFile(StateFile)
	if err != nil {
		return st
	}
	if json.Unmarshal(data, &st) != nil {
		return State{}
	}
	return st
}

func saveState(st State) error {
	data, _ := json.MarshalIndent(st, "", "  ")
	return writeFileAtomic(StateFile, append(data, '\n'))
}

var logMu sync.Mutex

// Log appends one JSON line: {"t": ..., k1: v1, k2: v2, ...} in the given order.
func Log(kv ...any) {
	var b strings.Builder
	b.WriteString(`{"t":`)
	t, _ := json.Marshal(time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	b.Write(t)
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := json.Marshal(fmt.Sprint(kv[i]))
		v, err := json.Marshal(kv[i+1])
		if err != nil {
			v, _ = json.Marshal(fmt.Sprint(kv[i+1]))
		}
		b.WriteByte(',')
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteString("}\n")
	logMu.Lock()
	defer logMu.Unlock()
	_ = os.MkdirAll(HomeDir, 0o755)
	f, err := os.OpenFile(LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(b.String())
}
