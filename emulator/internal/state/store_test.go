package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSaveLoadDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Load("sw"); ok || err != nil {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	d := Device{SwitchID: "sw", MAC: "02:4e:47:00:00:01", Adopted: true, AuthKey: "k", BootTime: time.Unix(1, 0).UTC(),
		Provisioned: map[string]json.RawMessage{"port_overrides": json.RawMessage(`[]`)}}
	if err := s.Save(d); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Load("sw")
	if err != nil || !ok || !reflect.DeepEqual(got, d) {
		t.Fatalf("round trip: %+v ok=%v err=%v", got, ok, err)
	}
	info, _ := os.Stat(filepath.Join(dir, "devices", "sw.json"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode %v, want 0600", info.Mode().Perm())
	}
	if ids, _ := s.IDs(); !reflect.DeepEqual(ids, []string{"sw"}) {
		t.Errorf("IDs = %v", ids)
	}
	if err := s.Delete("sw"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Load("sw"); ok {
		t.Error("state survived Delete")
	}
	if err := s.Delete("sw"); err != nil {
		t.Errorf("Delete of missing state must be a no-op: %v", err)
	}
}

func TestCorruptStateIsAnError(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	_ = os.WriteFile(filepath.Join(dir, "devices", "sw.json"), []byte("{"), 0o600)
	if _, _, err := s.Load("sw"); err == nil {
		t.Error("corrupt state must not be silently ignored")
	}
}
