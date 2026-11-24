package guestcfg

import (
	"bytes"
	"net/netip"
	"slices"
	"testing"
)

func TestEncodeDecode(t *testing.T) {
	c := Config{
		PrivateKey:   bytes.Repeat([]byte{1}, 32),
		HubPublicKey: bytes.Repeat([]byte{2}, 32),
		Endpoint:     "10.0.2.2:5000",
		Addresses:    []string{"10.89.1.2/24"},
		Routes:       []string{"10.89.0.0/16"},
		User:         "app:app",
		DNS:          "10.0.2.3",
	}
	s, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != c.Endpoint || !slices.Equal(got.Addresses, c.Addresses) || got.User != c.User || got.DNS != c.DNS || !bytes.Equal(got.PrivateKey, c.PrivateKey) {
		t.Fatalf("round trip = %+v", got)
	}
	if _, err := Decode("bm90LWpzb24"); err == nil {
		t.Fatal("Decode accepted garbage")
	}
}

const passwd = `root:x:0:0:root:/root:/bin/sh
# comment
nobody:x:65534:65534:nobody:/:/sbin/nologin
app:x:1000:1000::/home/app:/bin/sh
`

const group = `root:x:0:
wheel:x:10:root,app
app:x:1000:
docker:x:999:app
`

func TestResolveUser(t *testing.T) {
	cases := []struct {
		spec string
		want Identity
		err  bool
	}{
		{"app", Identity{UID: 1000, GID: 1000, Groups: []uint32{10, 999}, Home: "/home/app"}, false},
		{"1000", Identity{UID: 1000, GID: 1000, Groups: []uint32{10, 999}, Home: "/home/app"}, false},
		{"app:docker", Identity{UID: 1000, GID: 999, Groups: []uint32{10}, Home: "/home/app"}, false},
		{"1234", Identity{UID: 1234, GID: 0, Home: "/"}, false},
		{"1234:5", Identity{UID: 1234, GID: 5, Home: "/"}, false},
		{"nobody", Identity{UID: 65534, GID: 65534, Home: "/"}, false},
		{"ghost", Identity{}, true},
		{"app:ghosts", Identity{}, true},
	}
	for _, tc := range cases {
		got, err := ResolveUser(tc.spec, passwd, group)
		if tc.err {
			if err == nil {
				t.Errorf("%s: no error", tc.spec)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.spec, err)
			continue
		}
		if got.UID != tc.want.UID || got.GID != tc.want.GID || got.Home != tc.want.Home || !slices.Equal(got.Groups, tc.want.Groups) {
			t.Errorf("%s = %+v, want %+v", tc.spec, got, tc.want)
		}
	}
}

func TestResolvConf(t *testing.T) {
	const relay = "10.0.2.3"
	for _, c := range []struct {
		name, current string
		replace       bool
	}{
		// Debian's base image ships this one.
		{"the image's", "# https://1.1.1.1 (privacy-focused, highly-available DNS service)\nnameserver 1.1.1.1\nnameserver 1.0.0.1\n", true},
		{"the relay and another", "nameserver 10.0.2.3\nnameserver 8.8.8.8\n", true},
		{"none", "", true},
		{"no nameserver", "search example.com\n", true},
		{"the relay commented out", "# nameserver 10.0.2.3\nnameserver 1.1.1.1\n", true},
		// rungv's own, with the host's search and options.
		{"rungv's", "nameserver 10.0.2.3\nsearch tail1234.ts.net\noptions ndots:1\n", false},
		{"rungv's without a newline", "nameserver 10.0.2.3", false},
	} {
		got, replace := ResolvConf(c.current, relay)
		if got != "nameserver 10.0.2.3\n" || replace != c.replace {
			t.Errorf("%s: ResolvConf = %q, %v; want replace %v", c.name, got, replace, c.replace)
		}
	}
}

func TestParseExtraHost(t *testing.T) {
	cases := map[string]string{
		"db:10.0.0.5":                       "10.0.0.5\tdb",
		"db=10.0.0.5":                       "10.0.0.5\tdb",
		"host.docker.internal:host-gateway": "10.0.2.2\thost.docker.internal",
		"v6:::1":                            "::1\tv6",
		"v6=[2001:db8::1]":                  "2001:db8::1\tv6",
	}
	for in, want := range cases {
		got, err := ParseExtraHost(in, "10.0.2.2")
		if err != nil || got != want {
			t.Errorf("ParseExtraHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"db", ":1.2.3.4", "db:nope", "a b:1.2.3.4"} {
		if _, err := ParseExtraHost(bad, "10.0.2.2"); err == nil {
			t.Errorf("ParseExtraHost(%q) accepted", bad)
		}
	}
}

func TestDecodeWithoutLink(t *testing.T) {
	s, _ := Config{Hosts: []string{"10.0.0.5\tdb"}}.Encode()
	c, err := Decode(s)
	if err != nil || c.HasLink() || len(c.Hosts) != 1 {
		t.Fatalf("Decode = %+v, %v", c, err)
	}
}

func TestReconcileHelpers(t *testing.T) {
	p := netip.MustParsePrefix
	cur := []netip.Prefix{p("10.89.1.2/24"), p("10.89.2.2/24")}
	want := []netip.Prefix{p("10.89.2.2/24"), p("10.89.3.5/24"), p("172.30.0.5/16")}

	add, del := DiffPrefixes(cur, want)
	if !slices.Equal(add, []netip.Prefix{p("10.89.3.5/24"), p("172.30.0.5/16")}) {
		t.Errorf("add = %v", add)
	}
	if !slices.Equal(del, []netip.Prefix{p("10.89.1.2/24")}) {
		t.Errorf("del = %v", del)
	}

	// Disconnecting all networks removes every current address.
	add, del = DiffPrefixes(cur, nil)
	if len(add) != 0 || !slices.Equal(del, []netip.Prefix{p("10.89.1.2/24"), p("10.89.2.2/24")}) {
		t.Errorf("disconnect all: add=%v del=%v", add, del)
	}

	// UncoveredRoutes excludes subnets already covered by interface addresses.
	routes := []netip.Prefix{p("10.89.0.0/16"), p("172.30.0.0/16"), p("10.90.0.0/16")}
	uncovered := UncoveredRoutes(want, routes)
	if !slices.Equal(uncovered, []netip.Prefix{p("10.89.0.0/16"), p("10.90.0.0/16")}) {
		t.Errorf("uncovered = %v", uncovered)
	}

	// AllowedIPs combines masked addrs and routes without duplicates.
	allowed := AllowedIPs([]netip.Prefix{p("10.89.2.2/24"), p("172.30.0.5/16")}, routes)
	wantAllowed := []netip.Prefix{p("10.89.0.0/16"), p("10.89.2.0/24"), p("10.90.0.0/16"), p("172.30.0.0/16")}
	if !slices.Equal(allowed, wantAllowed) {
		t.Errorf("allowed = %v, want %v", allowed, wantAllowed)
	}
}
