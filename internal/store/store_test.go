package store

import "testing"

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
