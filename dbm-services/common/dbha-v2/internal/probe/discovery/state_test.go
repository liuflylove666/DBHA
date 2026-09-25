package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStartBootPersistsMonotonicGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	first, err := StartBoot(path, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.BootGeneration != 1 || first.BootID == "" {
		t.Fatalf("unexpected first boot: %+v", first)
	}
	first.Sequences["topology"] = 19
	if err := SaveState(path, first); err != nil {
		t.Fatal(err)
	}
	second, err := StartBoot(path, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if second.BootGeneration != 2 || second.BootID == first.BootID || second.Sequences["topology"] != 0 {
		t.Fatalf("unexpected second boot: %+v", second)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted State
	if err := json.Unmarshal(b, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.BootGeneration != second.BootGeneration {
		t.Fatal("boot generation not durable")
	}
	if err := StartBootWrongIdentity(path); err == nil {
		t.Fatal("accepted mismatched local agent identity")
	}
}

func StartBootWrongIdentity(path string) error { _, err := StartBoot(path, "agent-2"); return err }
