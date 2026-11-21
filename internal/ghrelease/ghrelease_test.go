package ghrelease

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "gvx-desktop" {
			t.Errorf("User-Agent %q", r.Header.Get("User-Agent"))
		}
		w.Write([]byte(`{"tag_name":"v1.2.3","html_url":"https://example.com/r","assets":[{"name":"a.zip","browser_download_url":"https://example.com/a.zip","size":12}],"body":"ignored"}`))
	}))
	defer srv.Close()
	r, err := Latest(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if r.Tag != "v1.2.3" || r.Page != "https://example.com/r" || len(r.Assets) != 1 {
		t.Fatalf("%+v", r)
	}
	if a, ok := r.Find("a.zip"); !ok || a.Size != 12 || a.URL != "https://example.com/a.zip" {
		t.Errorf("Find: %+v %v", a, ok)
	}
	if _, ok := r.Find("b.zip"); ok {
		t.Error("found a file that is not there")
	}
}

func TestLatestRefusesWhatIsNotARelease(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
	}{
		{http.StatusOK, `{"message":"no tag"}`},
		{http.StatusOK, `not json`},
		{http.StatusNotFound, `{"message":"Not Found"}`},
		{http.StatusForbidden, `{"message":"API rate limit exceeded"}`},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		}))
		if _, err := Latest(context.Background(), srv.URL); err == nil {
			t.Errorf("%d %s: no error", c.status, c.body)
		}
		srv.Close()
	}
}

func TestParseChecksum(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	for _, c := range []struct {
		file, name, want string
	}{
		{sum + "  app.dmg\n", "app.dmg", sum},
		{sum + " *app.dmg\n", "app.dmg", sum},
		{strings.Repeat("0", 64) + "  other\n" + strings.ToUpper(sum) + "  app.dmg\n", "app.dmg", sum},
		{sum + "\n", "app.dmg", sum},
		{sum + "  other.dmg\n", "app.dmg", ""},
		{"abc  app.dmg\n", "app.dmg", ""},
		{strings.Repeat("zz", 32) + "  app.dmg\n", "app.dmg", ""},
		{"", "app.dmg", ""},
	} {
		got, err := parseChecksum([]byte(c.file), c.name)
		if got != c.want || (err == nil) != (c.want != "") {
			t.Errorf("%q: %q, %v", c.file, got, err)
		}
	}
}

func serveFile(t *testing.T, body []byte, length bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !length {
			// No Content-Length: the size is known only as it arrives.
			w.Header().Set("Transfer-Encoding", "chunked")
			w.(http.Flusher).Flush()
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestDownload(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 1000)
	a := Asset{Name: "f", URL: serveFile(t, body, true)}
	var out bytes.Buffer
	var done, total int64
	if err := Download(context.Background(), a, sumOf(body), &out, 1<<20, func(d, n int64) { done, total = d, n }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), body) || done != 1000 || total != 1000 {
		t.Errorf("got %d bytes, progress %d of %d", out.Len(), done, total)
	}
}

func TestDownloadRefusesAMismatch(t *testing.T) {
	body := []byte("tampered")
	a := Asset{Name: "f", URL: serveFile(t, body, true)}
	err := Download(context.Background(), a, strings.Repeat("ab", 32), &bytes.Buffer{}, 1<<20, nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadIsCapped(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 4096)
	for _, length := range []bool{true, false} {
		a := Asset{Name: "f", URL: serveFile(t, body, length)}
		var out bytes.Buffer
		err := Download(context.Background(), a, sumOf(body), &out, 1024, nil)
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("length %v: err = %v", length, err)
		}
		if out.Len() > 1025 {
			t.Errorf("length %v: read %d bytes past the cap", length, out.Len())
		}
	}
	// What the API says of the size counts when the server does not say.
	a := Asset{Name: "f", URL: serveFile(t, []byte("small"), false), Size: 1 << 30}
	if err := Download(context.Background(), a, sumOf([]byte("small")), &bytes.Buffer{}, 1024, nil); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an asset the API says is too large: err = %v", err)
	}
}

func TestNewer(t *testing.T) {
	if !Newer("v0.17.2", "v0.9.10") || Newer("v0.9.10", "v0.17.2") || Newer("v1.0", "v1.0.0") || !Newer("v1", "(devel)") {
		t.Error("Newer orders versions wrongly")
	}
}
