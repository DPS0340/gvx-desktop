package kubeapi

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func readTestConfig(t *testing.T, name string) Config {
	t.Helper()
	k, err := ReadConfig("testdata/" + name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return k
}

// The file k3s writes: one context, a client certificate.
func TestReadK3sKubeconfig(t *testing.T) {
	k := readTestConfig(t, "k3s-kubeconfig.yaml")
	e, ok := k.Entry("")
	if !ok {
		t.Fatal("no current context")
	}
	want := Entry{Server: "https://127.0.0.1:6443", CA: "Q0EgUEVNCg==", Cert: "Q0VSVCBQRU0K", Key: "S0VZIFBFTQo="}
	if e != want {
		t.Errorf("entry %+v, want %+v", e, want)
	}
}

// kubectl config set-cluster, set-credentials (a certificate, and a token)
// and set-context wrote kubectl-kubeconfig.yaml (kubectl v1.37.2), and
// kubectl config view --raw -o json printed the same as kubectl-view.json:
// both read the same.
func TestKubectlWrittenKubeconfigReadsAsItsJSON(t *testing.T) {
	fromYAML := readTestConfig(t, "kubectl-kubeconfig.yaml")
	fromJSON := readTestConfig(t, "kubectl-view.json")
	if !reflect.DeepEqual(fromYAML, fromJSON) {
		t.Errorf("YAML %+v\nJSON %+v", fromYAML, fromJSON)
	}
	cert, _ := fromYAML.Entry("")
	if cert.Cert != "Q0VSVCBQRU0K" || cert.Key != "S0VZIFBFTQo=" || cert.CA != "Q0EgUEVNCg==" || cert.Token != "" {
		t.Errorf("the certificate entry %+v", cert)
	}
	tok, ok := fromYAML.Entry("token")
	want := Entry{Server: "https://localhost:6443", ServerName: "kubernetes", Token: "abc.def#1"}
	if !ok || tok != want {
		t.Errorf("the token entry %+v, want %+v", tok, want)
	}
	if _, ok := fromYAML.Entry("nothing"); ok {
		t.Error("an entry for a context that is not there")
	}
}

// A kubeconfig in another real file, when one is named: a check against
// kubeconfigs on hand that are not kept in the repository.
func TestReadKubeconfigNamedInTheEnvironment(t *testing.T) {
	yamlFile, jsonFile := os.Getenv("KUBEAPI_TEST_KUBECONFIG"), os.Getenv("KUBEAPI_TEST_KUBECONFIG_JSON")
	if yamlFile == "" || jsonFile == "" {
		t.Skip("KUBEAPI_TEST_KUBECONFIG and KUBEAPI_TEST_KUBECONFIG_JSON are not set")
	}
	a, err := ReadConfig(yamlFile)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ReadConfig(jsonFile)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("YAML %+v\nJSON %+v", a, b)
	}
}

func TestYAMLForms(t *testing.T) {
	src := strings.Join([]string{
		"---",
		"# a comment",
		"apiVersion: v1   # trailing",
		`current-context: "with: colon # not a comment"`,
		"clusters:",
		"  - name: 'it''s'",
		"    cluster:",
		"      server: https://10.0.0.1:6443",
		"",
		"contexts: []",
		"users:",
		"-",
		"  name: dashed",
		"  user:",
		`    token: "a\tb"`,
		"empty:",
		"list:",
		"- one",
		"- two",
	}, "\n")
	got, err := parseYAML(src)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"apiVersion":      "v1",
		"current-context": "with: colon # not a comment",
		"clusters":        []any{map[string]any{"name": "it's", "cluster": map[string]any{"server": "https://10.0.0.1:6443"}}},
		"contexts":        []any{},
		"users":           []any{map[string]any{"name": "dashed", "user": map[string]any{"token": "a\tb"}}},
		"empty":           nil,
		"list":            []any{"one", "two"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
}

func TestMalformedKubeconfigs(t *testing.T) {
	for name, src := range map[string]string{
		"tab":           "clusters:\n\t- name: x",
		"flow":          "clusters: [{name: x}]",
		"block scalar":  "token: |\n  abc",
		"anchor":        "users: &u\n- name: x",
		"unterminated":  `token: "abc`,
		"bad indent":    "clusters:\n  - name: x\n     cluster: y",
		"no key":        "apiVersion: v1\njust words",
		"duplicate":     "kind: Config\nkind: Config",
		"not a mapping": "- a\n- b",
		"bad JSON":      `{"clusters": [`,
		"wrong type":    "clusters: text",
	} {
		if _, err := ParseConfig([]byte(src)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if k, err := ParseConfig(nil); err != nil || len(k.Clusters) != 0 {
		t.Errorf("an empty file: %+v, %v", k, err)
	}
	if _, err := ReadConfig("testdata/none.yaml"); !os.IsNotExist(err) {
		t.Errorf("a missing file: %v", err)
	}
}
