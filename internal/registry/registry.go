// Package registry searches the image registries that can be searched, and
// lists the tags of what it finds. The OCI distribution API has no search:
// each registry answers in its own way, and most not at all.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Source is a registry Search can ask.
type Source string

const (
	DockerHub Source = "hub"
	Quay      Source = "quay"
)

// Sources lists the registries that can be searched, the default first.
func Sources() []Source { return []Source{DockerHub, Quay} }

// Title is the name people know the registry by.
func (s Source) Title() string {
	if s == Quay {
		return "Quay"
	}
	return "Docker Hub"
}

// The hosts asked; tests point them at servers of their own.
var (
	hubURL  = "https://hub.docker.com"
	quayURL = "https://quay.io"
)

// Endpoints points the package at other hosts than the registries' own,
// for a test, and returns what puts them back.
func Endpoints(hub, quay string) (restore func()) {
	oh, oq := hubURL, quayURL
	hubURL, quayURL = hub, quay
	return func() { hubURL, quayURL = oh, oq }
}

// Result is one repository a search found.
type Result struct {
	Source      Source
	Name        string // as its registry writes it: nginx, bitnami/redis, prometheus/prometheus
	Description string
	Stars       int
	Pulls       int64 // zero where the registry does not say
	Official    bool
}

// Ref is the reference that pulls r, without a tag.
func (r Result) Ref() string {
	if r.Source == Quay {
		return "quay.io/" + r.Name
	}
	return r.Name
}

// Searchable reports whether what is typed in an image field is words to
// search s for, rather than a reference to pull as it stands: one with a
// tag or digest, or naming a registry of its own, is the latter.
func Searchable(query string) bool {
	q := strings.TrimSpace(query)
	if len(q) < 2 || strings.ContainsAny(q, ":@ ") {
		return false
	}
	if first, _, ok := strings.Cut(q, "/"); ok && (strings.Contains(first, ".") || first == "localhost") {
		return false
	}
	return true
}

func getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "gvx-desktop")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", req.URL.Host, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v)
}

// Search asks s for the repositories that match query, the best first, at
// most limit of them.
func Search(ctx context.Context, s Source, query string, limit int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if s == Quay {
		return searchQuay(ctx, query, limit)
	}
	return searchHub(ctx, query, limit)
}

func searchHub(ctx context.Context, query string, limit int) ([]Result, error) {
	var body struct {
		Results []struct {
			Name        string `json:"repo_name"`
			Description string `json:"short_description"`
			Stars       int    `json:"star_count"`
			Pulls       int64  `json:"pull_count"`
			Official    bool   `json:"is_official"`
		} `json:"results"`
	}
	u := fmt.Sprintf("%s/v2/search/repositories/?query=%s&page_size=%d", hubURL, url.QueryEscape(query), limit)
	if err := getJSON(ctx, u, &body); err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(body.Results))
	for _, r := range body.Results {
		out = append(out, Result{Source: DockerHub, Name: r.Name, Description: strings.TrimSpace(r.Description),
			Stars: r.Stars, Pulls: r.Pulls, Official: r.Official})
	}
	// The Hub does not always put its official image first; a search for
	// redis should.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Official && !out[j].Official })
	return out, nil
}

func searchQuay(ctx context.Context, query string, limit int) ([]Result, error) {
	var body struct {
		Results []struct {
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			Namespace struct {
				Name string `json:"name"`
			} `json:"namespace"`
			Description *string `json:"description"`
			Public      bool    `json:"is_public"`
			Stars       int     `json:"stars"`
		} `json:"results"`
	}
	u := fmt.Sprintf("%s/api/v1/find/repositories?query=%s&page=1", quayURL, url.QueryEscape(query))
	if err := getJSON(ctx, u, &body); err != nil {
		return nil, err
	}
	var out []Result
	for _, r := range body.Results {
		if r.Kind != "repository" || !r.Public || len(out) == limit {
			continue
		}
		res := Result{Source: Quay, Name: r.Namespace.Name + "/" + r.Name, Stars: r.Stars}
		if r.Description != nil {
			// A description is Markdown, of which the first line says enough.
			res.Description, _, _ = strings.Cut(strings.TrimSpace(*r.Description), "\n")
			res.Description = strings.TrimLeft(res.Description, "# ")
		}
		out = append(out, res)
	}
	return out, nil
}

// Tags lists the tags of r that changed last, at most limit of them. From
// Docker Hub, which says what each tag was built for, only those with an
// image for linux on arch are listed.
func Tags(ctx context.Context, r Result, arch string, limit int) ([]string, error) {
	if r.Source == Quay {
		var body struct {
			Tags []struct {
				Name string `json:"name"`
			} `json:"tags"`
		}
		u := fmt.Sprintf("%s/api/v1/repository/%s/tag/?limit=%d&onlyActiveTags=true", quayURL, r.Name, limit)
		if err := getJSON(ctx, u, &body); err != nil {
			return nil, err
		}
		out := make([]string, 0, len(body.Tags))
		for _, t := range body.Tags {
			out = append(out, t.Name)
		}
		return out, nil
	}
	name := r.Name
	if !strings.Contains(name, "/") {
		name = "library/" + name
	}
	var body struct {
		Results []struct {
			Name   string `json:"name"`
			Images []struct {
				OS   string `json:"os"`
				Arch string `json:"architecture"`
			} `json:"images"`
		} `json:"results"`
	}
	// Asked for more than wanted, since some are for other machines.
	u := fmt.Sprintf("%s/v2/repositories/%s/tags?page_size=%d&ordering=last_updated", hubURL, name, min(100, limit*3))
	if err := getJSON(ctx, u, &body); err != nil {
		return nil, err
	}
	var out []string
	for _, t := range body.Results {
		fits := len(t.Images) == 0
		for _, img := range t.Images {
			if img.OS == "linux" && img.Arch == arch {
				fits = true
			}
		}
		if fits && len(out) < limit {
			out = append(out, t.Name)
		}
	}
	return out, nil
}
