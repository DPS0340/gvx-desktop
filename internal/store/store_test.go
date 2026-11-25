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

func TestSettingsLabsDefaultsAndPorts(t *testing.T) {
	def := DefaultSettings()
	if def.LocalProxy {
		t.Fatal("DefaultSettings().LocalProxy = true, want false")
	}
	if def.MCPServer {
		t.Fatal("DefaultSettings().MCPServer = true, want false")
	}
	if def.LocalProxyHTTPPort != 0 || def.LocalProxyHTTPSPort != 0 || def.MCPPort != 0 {
		t.Fatalf("DefaultSettings() ports = (%d, %d, %d), want (0, 0, 0)",
			def.LocalProxyHTTPPort, def.LocalProxyHTTPSPort, def.MCPPort)
	}
	if DefaultLocalProxyHTTPPort != 18080 {
		t.Fatalf("DefaultLocalProxyHTTPPort = %d, want 18080", DefaultLocalProxyHTTPPort)
	}
	if DefaultLocalProxyHTTPSPort != 18443 {
		t.Fatalf("DefaultLocalProxyHTTPSPort = %d, want 18443", DefaultLocalProxyHTTPSPort)
	}
	if DefaultMCPPort != 18787 {
		t.Fatalf("DefaultMCPPort = %d, want 18787", DefaultMCPPort)
	}
	if got := def.LocalProxyHTTPPortOrDefault(); got != DefaultLocalProxyHTTPPort {
		t.Fatalf("DefaultSettings().LocalProxyHTTPPortOrDefault() = %d, want %d", got, DefaultLocalProxyHTTPPort)
	}
	if got := def.LocalProxyHTTPSPortOrDefault(); got != DefaultLocalProxyHTTPSPort {
		t.Fatalf("DefaultSettings().LocalProxyHTTPSPortOrDefault() = %d, want %d", got, DefaultLocalProxyHTTPSPort)
	}
	if got := def.MCPPortOrDefault(); got != DefaultMCPPort {
		t.Fatalf("DefaultSettings().MCPPortOrDefault() = %d, want %d", got, DefaultMCPPort)
	}

	portCases := []struct {
		in        int
		wantHTTP  int
		wantHTTPS int
		wantMCP   int
	}{
		{-100, DefaultLocalProxyHTTPPort, DefaultLocalProxyHTTPSPort, DefaultMCPPort},
		{-1, DefaultLocalProxyHTTPPort, DefaultLocalProxyHTTPSPort, DefaultMCPPort},
		{0, DefaultLocalProxyHTTPPort, DefaultLocalProxyHTTPSPort, DefaultMCPPort},
		{1, 1, 1, 1},
		{8080, 8080, 8080, 8080},
		{65535, 65535, 65535, 65535},
		{65536, DefaultLocalProxyHTTPPort, DefaultLocalProxyHTTPSPort, DefaultMCPPort},
		{100000, DefaultLocalProxyHTTPPort, DefaultLocalProxyHTTPSPort, DefaultMCPPort},
	}
	for _, tc := range portCases {
		s := Settings{
			LocalProxyHTTPPort:  tc.in,
			LocalProxyHTTPSPort: tc.in,
			MCPPort:             tc.in,
		}
		if got := s.LocalProxyHTTPPortOrDefault(); got != tc.wantHTTP {
			t.Errorf("LocalProxyHTTPPortOrDefault(%d) = %d, want %d", tc.in, got, tc.wantHTTP)
		}
		if got := s.LocalProxyHTTPSPortOrDefault(); got != tc.wantHTTPS {
			t.Errorf("LocalProxyHTTPSPortOrDefault(%d) = %d, want %d", tc.in, got, tc.wantHTTPS)
		}
		if got := s.MCPPortOrDefault(); got != tc.wantMCP {
			t.Errorf("MCPPortOrDefault(%d) = %d, want %d", tc.in, got, tc.wantMCP)
		}
	}
}

func TestSettingsLabsJSONOmitEmptyAndStoreRoundTrip(t *testing.T) {
	labsKeys := []string{
		`"localProxy"`,
		`"localProxyHttpPort"`,
		`"localProxyHttpsPort"`,
		`"mcpServer"`,
		`"mcpPort"`,
	}

	// Zero-value Labs fields must be omitted for clean backwards-compatible JSON.
	rawDefault, err := json.Marshal(DefaultSettings())
	if err != nil {
		t.Fatalf("json.Marshal(DefaultSettings()): %v", err)
	}
	for _, key := range labsKeys {
		if strings.Contains(string(rawDefault), key) {
			t.Errorf("DefaultSettings JSON %s unexpectedly contains %s", rawDefault, key)
		}
	}

	st := Store{Dir: t.TempDir()}

	initial, err := st.Settings()
	if err != nil {
		t.Fatalf("Settings() on empty dir: %v", err)
	}
	if initial.LocalProxy || initial.MCPServer ||
		initial.LocalProxyHTTPPort != 0 || initial.LocalProxyHTTPSPort != 0 || initial.MCPPort != 0 {
		t.Fatalf("initial Labs settings not zero: %+v", initial)
	}

	// Enable Labs features with custom ports and verify JSON keys + store round-trip.
	custom := DefaultSettings()
	custom.LocalProxy = true
	custom.LocalProxyHTTPPort = 28080
	custom.LocalProxyHTTPSPort = 28443
	custom.MCPServer = true
	custom.MCPPort = 28787

	rawCustom, err := json.Marshal(custom)
	if err != nil {
		t.Fatalf("json.Marshal(custom): %v", err)
	}
	for _, key := range labsKeys {
		if !strings.Contains(string(rawCustom), key) {
			t.Errorf("custom Settings JSON %s missing %s", rawCustom, key)
		}
	}

	if err := st.SaveSettings(custom); err != nil {
		t.Fatalf("SaveSettings(custom): %v", err)
	}
	reloaded, err := st.Settings()
	if err != nil {
		t.Fatalf("Settings() after save custom: %v", err)
	}
	if !custom.Equal(reloaded) {
		t.Fatalf("store round-trip mismatch:\ngot:  %+v\nwant: %+v", reloaded, custom)
	}
	if reloaded.LocalProxyHTTPPortOrDefault() != 28080 ||
		reloaded.LocalProxyHTTPSPortOrDefault() != 28443 ||
		reloaded.MCPPortOrDefault() != 28787 {
		t.Fatalf("reloaded helper ports = (%d, %d, %d), want (28080, 28443, 28787)",
			reloaded.LocalProxyHTTPPortOrDefault(),
			reloaded.LocalProxyHTTPSPortOrDefault(),
			reloaded.MCPPortOrDefault())
	}

	// Turning Labs features back off and clearing ports persists cleanly.
	reset := DefaultSettings()
	if err := st.SaveSettings(reset); err != nil {
		t.Fatalf("SaveSettings(reset): %v", err)
	}
	reloadedReset, err := st.Settings()
	if err != nil {
		t.Fatalf("Settings() after save reset: %v", err)
	}
	if !reset.Equal(reloadedReset) {
		t.Fatalf("store reset round-trip mismatch:\ngot:  %+v\nwant: %+v", reloadedReset, reset)
	}
}

func TestSettingsLabsEqual(t *testing.T) {
	base := DefaultSettings()

	mutations := []struct {
		name   string
		mutate func(*Settings)
	}{
		{"LocalProxy", func(s *Settings) { s.LocalProxy = true }},
		{"LocalProxyHTTPPort", func(s *Settings) { s.LocalProxyHTTPPort = 18081 }},
		{"LocalProxyHTTPSPort", func(s *Settings) { s.LocalProxyHTTPSPort = 18444 }},
		{"MCPServer", func(s *Settings) { s.MCPServer = true }},
		{"MCPPort", func(s *Settings) { s.MCPPort = 18788 }},
	}

	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			a := DefaultSettings()
			b := DefaultSettings()
			tc.mutate(&a)
			tc.mutate(&b)
			if base.Equal(a) {
				t.Errorf("Settings with mutated %s should not be Equal to DefaultSettings", tc.name)
			}
			if !a.Equal(b) {
				t.Errorf("Settings with matching %s should be Equal", tc.name)
			}
		})
	}
}

func TestSettingsMicroVMDefaultsHelpersAndRoundTrip(t *testing.T) {
	def := DefaultSettings()
	if def.MicroVMContainers {
		t.Fatal("DefaultSettings().MicroVMContainers = true, want false")
	}
	if def.MicroVMCPUs != 0 || def.MicroVMMemoryMiB != 0 {
		t.Fatalf("DefaultSettings() micro-VM resources = (%d, %d), want (0, 0)", def.MicroVMCPUs, def.MicroVMMemoryMiB)
	}
	if DefaultMicroVMCPUs != 4 {
		t.Fatalf("DefaultMicroVMCPUs = %d, want 4", DefaultMicroVMCPUs)
	}
	if DefaultMicroVMMemoryMiB != 1024 {
		t.Fatalf("DefaultMicroVMMemoryMiB = %d, want 1024", DefaultMicroVMMemoryMiB)
	}
	if got := def.MicroVMCPUsOrDefault(); got != DefaultMicroVMCPUs {
		t.Fatalf("DefaultSettings().MicroVMCPUsOrDefault() = %d, want %d", got, DefaultMicroVMCPUs)
	}
	if got := def.MicroVMMemoryMiBOrDefault(); got != DefaultMicroVMMemoryMiB {
		t.Fatalf("DefaultSettings().MicroVMMemoryMiBOrDefault() = %d, want %d", got, DefaultMicroVMMemoryMiB)
	}

	for _, tc := range []struct {
		cpus    int
		mem     int
		wantCPU int
		wantMem int
	}{
		{-2, -512, DefaultMicroVMCPUs, DefaultMicroVMMemoryMiB},
		{0, 0, DefaultMicroVMCPUs, DefaultMicroVMMemoryMiB},
		{0, 64, DefaultMicroVMCPUs, DefaultMicroVMMemoryMiB},
		{0, 127, DefaultMicroVMCPUs, DefaultMicroVMMemoryMiB},
		{1, 128, 1, 128},
		{2, 512, 2, 512},
		{8, 4096, 8, 4096},
	} {
		s := Settings{MicroVMCPUs: tc.cpus, MicroVMMemoryMiB: tc.mem}
		if got := s.MicroVMCPUsOrDefault(); got != tc.wantCPU {
			t.Errorf("MicroVMCPUsOrDefault(%d) = %d, want %d", tc.cpus, got, tc.wantCPU)
		}
		if got := s.MicroVMMemoryMiBOrDefault(); got != tc.wantMem {
			t.Errorf("MicroVMMemoryMiBOrDefault(%d) = %d, want %d", tc.mem, got, tc.wantMem)
		}
	}

	// Zero values omitted from JSON for backwards compatibility.
	microVMKeys := []string{`"microVmContainers"`, `"microVmCpus"`, `"microVmMemoryMiB"`}
	rawDef, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("json.Marshal(def): %v", err)
	}
	for _, key := range microVMKeys {
		if strings.Contains(string(rawDef), key) {
			t.Errorf("DefaultSettings JSON %s unexpectedly contains %s", rawDef, key)
		}
	}

	// Store round-trip and Equal.
	st := Store{Dir: t.TempDir()}
	custom := DefaultSettings()
	custom.MicroVMContainers = true
	custom.MicroVMCPUs = 6
	custom.MicroVMMemoryMiB = 2048

	rawCustom, err := json.Marshal(custom)
	if err != nil {
		t.Fatalf("json.Marshal(custom): %v", err)
	}
	for _, key := range microVMKeys {
		if !strings.Contains(string(rawCustom), key) {
			t.Errorf("custom JSON %s missing %s", rawCustom, key)
		}
	}
	if err := st.SaveSettings(custom); err != nil {
		t.Fatalf("SaveSettings(custom): %v", err)
	}
	reloaded, err := st.Settings()
	if err != nil {
		t.Fatalf("Settings() after save: %v", err)
	}
	if !custom.Equal(reloaded) {
		t.Fatalf("store round-trip mismatch:\ngot:  %+v\nwant: %+v", reloaded, custom)
	}
	if def.Equal(custom) {
		t.Fatal("DefaultSettings should not Equal custom micro-VM Settings")
	}
}

func TestSettingsDeveloperModeDefaultOmitEmptyAndRoundTrip(t *testing.T) {
	def := DefaultSettings()
	if def.DeveloperMode {
		t.Fatal("DefaultSettings().DeveloperMode = true, want false")
	}

	rawDef, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("json.Marshal(DefaultSettings()): %v", err)
	}
	if strings.Contains(string(rawDef), `"developerMode"`) {
		t.Errorf("DefaultSettings JSON %s unexpectedly contains developerMode", rawDef)
	}

	custom := DefaultSettings()
	custom.DeveloperMode = true
	if def.Equal(custom) {
		t.Fatal("DefaultSettings should not Equal Settings with DeveloperMode=true")
	}
	custom2 := DefaultSettings()
	custom2.DeveloperMode = true
	if !custom.Equal(custom2) {
		t.Fatal("Settings with matching DeveloperMode=true should be Equal")
	}

	rawCustom, err := json.Marshal(custom)
	if err != nil {
		t.Fatalf("json.Marshal(custom): %v", err)
	}
	if !strings.Contains(string(rawCustom), `"developerMode":true`) {
		t.Errorf("custom JSON %s missing developerMode:true", rawCustom)
	}

	st := Store{Dir: t.TempDir()}
	if err := st.SaveSettings(custom); err != nil {
		t.Fatalf("SaveSettings(custom): %v", err)
	}
	reloaded, err := st.Settings()
	if err != nil {
		t.Fatalf("Settings() after save: %v", err)
	}
	if !reloaded.DeveloperMode {
		t.Fatal("reloaded.DeveloperMode = false, want true")
	}
	if !custom.Equal(reloaded) {
		t.Fatalf("store round-trip mismatch:\ngot:  %+v\nwant: %+v", reloaded, custom)
	}
}
