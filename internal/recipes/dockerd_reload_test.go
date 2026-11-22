package recipes

import "testing"

func TestDockerdReloadResult(t *testing.T) {
	for _, tc := range []struct {
		line string
		ok   bool
		err  string
	}{
		{`time="2026-11-23T01:02:03Z" level=info msg="Got signal to reload configuration, reloading from: /etc/docker/daemon.json"`, false, ""},
		// What docker:dind (dockerd 28) wrote in TestDockerEngineHarness.
		{`time="2026-11-22T19:13:38.879440225Z" level=info msg="Got signal to reload configuration" config-file=/etc/docker/daemon.json`, false, ""},
		{`time="2026-11-22T19:13:38.917079884Z" level=info msg="Reloaded configuration" config="{\"labels\":[\"gvxd=1\"],\"debug\":true}"`, true, ""},
		{`time="2026-11-22T19:13:39.218904689Z" level=error msg="Error reloading configuration" error="unable to configure the Docker daemon with file /etc/docker/daemon.json: invalid JSON: unexpected end of JSON input"`, false, "unable to configure the Docker daemon with file /etc/docker/daemon.json: invalid JSON: unexpected end of JSON input"},
		{`time="2026-11-22T19:13:39Z" level=error msg="Error reconfiguring the daemon: \"x\" is bad"`, false, `Error reconfiguring the daemon: "x" is bad`},
	} {
		ok, err := dockerdReloadResult(tc.line)
		if ok != tc.ok || (err == nil) != (tc.err == "") || err != nil && err.Error() != tc.err {
			t.Errorf("%s: %v %v", tc.line, ok, err)
		}
	}
}
