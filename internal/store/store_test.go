package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSettingsSandboxEngineDefaultAndCompatPersistence(t *testing.T) {
	st := Store{Dir: t.TempDir()}

	// First launch defaults to the native sandbox engine.
	set, err := st.Settings()
	if err != nil {
		t.Fatalf("Settings() on empty dir: %v", err)
	}
	if !set.SandboxEngine {
		t.Fatal("DefaultSettings().SandboxEngine = false, want true")
	}

	// Switching to compatibility mode (SandboxEngine = false) must persist
	// explicitly rather than reverting to the true default on reload.
	set.SandboxEngine = false
	if err := st.SaveSettings(set); err != nil {
		t.Fatalf("SaveSettings(false): %v", err)
	}
	reloaded, err := st.Settings()
	if err != nil {
		t.Fatalf("Settings() after save: %v", err)
	}
	if reloaded.SandboxEngine {
		t.Fatal("reloaded SandboxEngine = true, want false (compatibility mode)")
	}
}

func TestSettingsLocalRegistryMode(t *testing.T) {
	if got := DefaultSettings().LocalRegistryMode(); got != LocalRegistryAuto {
		t.Fatalf("DefaultSettings().LocalRegistryMode() = %q, want %q", got, LocalRegistryAuto)
	}

	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"", LocalRegistryAuto},
		{LocalRegistryAuto, LocalRegistryAuto},
		{LocalRegistryOn, LocalRegistryOn},
		{LocalRegistryOff, LocalRegistryOff},
		{"unknown", LocalRegistryAuto},
	} {
		set := Settings{LocalRegistry: tc.raw}
		if got := set.LocalRegistryMode(); got != tc.want {
			t.Errorf("Settings{LocalRegistry: %q}.LocalRegistryMode() = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestSettingsLocalRegistryJSONRoundTripAndStore(t *testing.T) {
	st := Store{Dir: t.TempDir()}

	initial, err := st.Settings()
	if err != nil {
		t.Fatalf("Settings() on empty dir: %v", err)
	}
	if initial.LocalRegistry != "" || initial.LocalRegistryMode() != LocalRegistryAuto {
		t.Fatalf("initial LocalRegistry = %q (mode %q), want \"\" (mode %q)", initial.LocalRegistry, initial.LocalRegistryMode(), LocalRegistryAuto)
	}

	for _, tc := range []struct {
		name     string
		value    string
		wantMode string
		wantJSON bool
	}{
		{"empty", "", LocalRegistryAuto, false},
		{"auto", LocalRegistryAuto, LocalRegistryAuto, true},
		{"on", LocalRegistryOn, LocalRegistryOn, true},
		{"off", LocalRegistryOff, LocalRegistryOff, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := DefaultSettings()
			in.LocalRegistry = tc.value

			raw, err := json.Marshal(in)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			hasField := strings.Contains(string(raw), `"localRegistry"`)
			if hasField != tc.wantJSON {
				t.Errorf("JSON %s: has localRegistry = %v, want %v", raw, hasField, tc.wantJSON)
			}
			var decoded Settings
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			if decoded.LocalRegistry != tc.value || decoded.LocalRegistryMode() != tc.wantMode {
				t.Errorf("JSON round-trip = %q (mode %q), want %q (mode %q)", decoded.LocalRegistry, decoded.LocalRegistryMode(), tc.value, tc.wantMode)
			}
			if !in.Equal(decoded) {
				t.Errorf("JSON round-trip Settings not Equal: in=%+v decoded=%+v", in, decoded)
			}

			if err := st.SaveSettings(in); err != nil {
				t.Fatalf("SaveSettings(%q): %v", tc.value, err)
			}
			reloaded, err := st.Settings()
			if err != nil {
				t.Fatalf("Settings() after save %q: %v", tc.value, err)
			}
			if reloaded.LocalRegistry != tc.value || reloaded.LocalRegistryMode() != tc.wantMode {
				t.Errorf("store round-trip = %q (mode %q), want %q (mode %q)", reloaded.LocalRegistry, reloaded.LocalRegistryMode(), tc.value, tc.wantMode)
			}
			if !in.Equal(reloaded) {
				t.Errorf("store round-trip Settings not Equal: in=%+v reloaded=%+v", in, reloaded)
			}
		})
	}
}

func TestSettingsLocalRegistryEqual(t *testing.T) {
	base := DefaultSettings()
	same := DefaultSettings()
	if !base.Equal(same) {
		t.Fatal("identical DefaultSettings should be Equal")
	}

	for _, mode := range []string{LocalRegistryAuto, LocalRegistryOn, LocalRegistryOff} {
		a := DefaultSettings()
		b := DefaultSettings()
		a.LocalRegistry = mode
		b.LocalRegistry = mode
		if !a.Equal(b) {
			t.Errorf("Settings with matching LocalRegistry=%q should be Equal", mode)
		}
		if base.Equal(a) {
			t.Errorf("Settings with LocalRegistry=\"\" and %q should not be Equal", mode)
		}
	}

	on := DefaultSettings()
	on.LocalRegistry = LocalRegistryOn
	off := DefaultSettings()
	off.LocalRegistry = LocalRegistryOff
	if on.Equal(off) {
		t.Fatal("Settings with LocalRegistry=on and LocalRegistry=off should not be Equal")
	}
}
