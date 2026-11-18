package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestSearchable(t *testing.T) {
	for q, want := range map[string]bool{
		"nginx": true, "bitnami/redis": true, "  postgres ": true,
		"n": false, "": false, "nginx:alpine": false, "nginx@sha256:abc": false,
		"ghcr.io/org/app": false, "localhost/app": false, "two words": false,
	} {
		if got := Searchable(q); got != want {
			t.Errorf("Searchable(%q) = %v", q, got)
		}
	}
}

func serve(t *testing.T, routes map[string]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	oh, oq := hubURL, quayURL
	hubURL, quayURL = srv.URL, srv.URL
	t.Cleanup(func() { hubURL, quayURL = oh, oq })
}

func TestSearchDockerHub(t *testing.T) {
	serve(t, map[string]string{
		"/v2/search/repositories/": `{"results":[
			{"repo_name":"nginx","short_description":" Official build of Nginx. ","star_count":21422,"pull_count":13593185359,"is_official":true},
			{"repo_name":"nginx/nginx-ingress","short_description":"","star_count":124,"pull_count":10,"is_official":false}]}`,
		"/v2/repositories/library/nginx/tags": `{"results":[
			{"name":"alpine","images":[{"os":"linux","architecture":"amd64"},{"os":"linux","architecture":"arm64"}]},
			{"name":"windows","images":[{"os":"windows","architecture":"amd64"}]},
			{"name":"amd-only","images":[{"os":"linux","architecture":"amd64"}]},
			{"name":"bare","images":[]}]}`,
	})
	got, err := Search(context.Background(), DockerHub, " nginx ", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "nginx" || !got[0].Official || got[0].Pulls != 13593185359 ||
		got[0].Description != "Official build of Nginx." || got[0].Ref() != "nginx" || got[1].Ref() != "nginx/nginx-ingress" {
		t.Errorf("results: %+v", got)
	}
	tags, err := Tags(context.Background(), got[0], "arm64", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tags, []string{"alpine", "bare"}) {
		t.Errorf("tags: %v", tags)
	}
}

func TestSearchQuay(t *testing.T) {
	serve(t, map[string]string{
		"/api/v1/find/repositories": `{"results":[
			{"kind":"repository","name":"prometheus","namespace":{"name":"prometheus"},"description":"# Prometheus\nmore","is_public":true,"stars":7},
			{"kind":"repository","name":"secret","namespace":{"name":"corp"},"description":null,"is_public":false},
			{"kind":"repository","name":"b","namespace":{"name":"a"},"description":null,"is_public":true}]}`,
		"/api/v1/repository/prometheus/prometheus/tag/": `{"tags":[{"name":"main"},{"name":"v3.0.0"}]}`,
	})
	got, err := Search(context.Background(), Quay, "prometheus", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Ref() != "quay.io/prometheus/prometheus" || got[0].Description != "Prometheus" || got[1].Name != "a/b" {
		t.Errorf("results: %+v", got)
	}
	tags, err := Tags(context.Background(), got[0], "arm64", 10)
	if err != nil || !slices.Equal(tags, []string{"main", "v3.0.0"}) {
		t.Errorf("tags: %v, %v", tags, err)
	}
}

func TestSearchReportsARefusal(t *testing.T) {
	serve(t, nil)
	if _, err := Search(context.Background(), DockerHub, "nginx", 10); err == nil {
		t.Error("no error for a 404")
	}
}
