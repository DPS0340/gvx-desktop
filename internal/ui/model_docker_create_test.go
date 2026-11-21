package ui

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestWindowsSource(t *testing.T) {
	for _, c := range []struct{ in, src, rest string }{
		{`C:\Users\me\src:/app`, `C:\Users\me\src`, ":/app"},
		{`c:/Users/me/src:/app:ro`, `c:/Users/me/src`, ":/app:ro"},
		{`D:\data`, `D:\data`, ""},
		{"data:/var/lib", "data", ":/var/lib"},
	} {
		if src, rest := windowsSource(c.in); src != c.src || rest != c.rest {
			t.Errorf("%q: %q %q", c.in, src, rest)
		}
	}
}

func TestWindowsBinds(t *testing.T) {
	body := `{"Image":"a<b","HostConfig":{"Memory":9007199254740993,"Binds":["C:\\Users\\me\\src:/app:ro","data:/var/lib","/already:/x"],` +
		`"Mounts":[{"Type":"bind","Source":"D:\\work","Target":"/w"},{"Type":"volume","Source":"vol","Target":"/v"}]}}`
	out := windowsBinds([]byte(body))
	if out == nil {
		t.Fatal("nothing changed")
	}
	if !strings.Contains(string(out), `"Memory":9007199254740993`) || !strings.Contains(string(out), `"a<b"`) {
		t.Errorf("what was not a path changed: %s", out)
	}
	var got struct {
		HostConfig struct {
			Binds  []string
			Mounts []struct{ Type, Source, Target string }
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.HostConfig.Binds, []string{"/c/Users/me/src:/app:ro", "data:/var/lib", "/already:/x"}) {
		t.Errorf("binds %v", got.HostConfig.Binds)
	}
	if m := got.HostConfig.Mounts; m[0].Source != "/d/work" || m[1].Source != "vol" {
		t.Errorf("mounts %+v", m)
	}
	for _, same := range []string{`{`, `{}`, `{"HostConfig":{"Binds":["data:/x","/a:/b","C:","1:\\x:/y"]}}`} {
		if out := windowsBinds([]byte(same)); out != nil {
			t.Errorf("%s changed to %s", same, out)
		}
	}
}
