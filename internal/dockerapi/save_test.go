package dockerapi

import (
	"context"
	"io"
	"net/http"
	"testing"
)

func TestSaveImage(t *testing.T) {
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/images/ghcr.io/acme/api:dev/get":
			_, _ = w.Write([]byte("tar bytes"))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"reference does not exist"}`))
		}
	})
	rc, err := c.SaveImage(context.Background(), "ghcr.io/acme/api:dev")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "tar bytes" {
		t.Errorf("saved %q", b)
	}
	if _, err := c.SaveImage(context.Background(), "nope:1"); err == nil || err.Error() != "reference does not exist" {
		t.Errorf("a missing image: %v", err)
	}
}
