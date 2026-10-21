package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestRoundTripAndWrongKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.enc")
	s, err := Open(path, key(1))
	if err != nil {
		t.Fatal(err)
	}
	if s.Get().LoggedIn() {
		t.Fatal("fresh state is logged in")
	}
	if err := s.Update(func(st *State) {
		st.RefreshToken, st.DeviceKey, st.Target = "v1.3.secret", []byte{1, 2, 3}, "eli"
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("secret")) {
		t.Fatal("state file holds the token in clear")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	s2, err := Open(path, key(1))
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get(); got.RefreshToken != "v1.3.secret" || got.Target != "eli" || !bytes.Equal(got.DeviceKey, []byte{1, 2, 3}) {
		t.Fatalf("reloaded %+v", got)
	}
	if _, err := Open(path, key(2)); err == nil {
		t.Fatal("opened with the wrong key")
	}
}

func TestGetIsACopy(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "s"), key(1))
	_ = s.Update(func(st *State) { st.DeviceKey = []byte{9} })
	g := s.Get()
	g.DeviceKey[0] = 0
	if s.Get().DeviceKey[0] != 9 {
		t.Fatal("Get leaked the internal slice")
	}
}

func TestClearSessionKeepsDeviceAndBotState(t *testing.T) {
	st := State{DeviceID: "d", RefreshToken: "r", UserID: "u", Target: "saved", TGOffset: 7}
	st.ClearSession()
	if st.LoggedIn() || st.UserID != "" || st.DeviceID != "d" || st.Target != "saved" || st.TGOffset != 7 {
		t.Fatalf("%+v", st)
	}
}
