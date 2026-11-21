package engine

// Owner: frozen (architect). Helpers every workstream's handlers use.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// apiError is an error with the HTTP status the API answers it with.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

// errNotFound is a 404: No such container: x, No such image: x, ...
func errNotFound(format string, args ...any) error {
	return &apiError{http.StatusNotFound, fmt.Sprintf(format, args...)}
}

// errConflict is a 409: a name in use, a container that runs, ...
func errConflict(format string, args ...any) error {
	return &apiError{http.StatusConflict, fmt.Sprintf(format, args...)}
}

// errInvalid is a 400: a request the engine cannot make sense of.
func errInvalid(format string, args ...any) error {
	return &apiError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

// errNotModified is a 304: start of a running container, stop of a stopped one.
var errNotModified = &apiError{http.StatusNotModified, ""}

// errNotSupported is a 501 for what the engine does not do.
func errNotSupported(what string) error {
	return &apiError{http.StatusNotImplemented, notSupportedMessage(what)}
}

func notSupportedMessage(what string) string {
	return what + " is not supported by GVX Desktop's experimental sandbox engine yet"
}

// errBuildDriverNotSupported is a 501 for Docker's built-in BuildKit driver
// endpoints (/grpc, /session) and buildx's docker-container fallback.
func errBuildDriverNotSupported() error {
	return &apiError{http.StatusNotImplemented, buildDriverNotSupportedMessage()}
}

func buildDriverNotSupportedMessage() string {
	return "Docker's built-in BuildKit driver is not supported by GVX Desktop's experimental sandbox engine; builds go through the " + BuilderName + " builder instead (select it with: docker buildx use --default " + BuilderName + ")"
}

// statusOf is the HTTP status of err: an apiError's, or 500.
func statusOf(err error) int {
	if e, ok := errors.AsType[*apiError](err); ok {
		return e.status
	}
	return http.StatusInternalServerError
}

// writeError answers err as the Docker API does, {"message": "..."}.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := statusOf(err)
	if status == http.StatusNotModified {
		w.WriteHeader(status)
		return
	}
	body, _ := json.Marshal(struct {
		Message string `json:"message"`
	}{err.Error()})
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// writeJSON answers v as JSON with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// decodeBody reads a JSON request body into v; an empty body leaves v as
// it is.
func decodeBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		return errInvalid("%v", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errInvalid("invalid JSON: %v", err)
	}
	return nil
}

// queryBool reads a boolean query parameter as dockerd does: anything but
// "", "0", "no", "false" and "none" is true.
func queryBool(r *http.Request, name string) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name))) {
	case "", "0", "no", "false", "none":
		return false
	}
	return true
}

// queryInt reads an integer query parameter, def when absent.
func queryInt(r *http.Request, name string, def int) (int, error) {
	v := strings.TrimSpace(r.URL.Query().Get(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, errInvalid("invalid value for %s: %q", name, v)
	}
	return n, nil
}

// filters are the filters= query parameter of the list endpoints, by key.
type filters map[string][]string

// parseFilters reads filters= in both forms clients send:
// {"label":{"a=b":true}} (Compose, the CLI) and {"label":["a=b"]} (older).
func parseFilters(raw string) (filters, error) {
	f := filters{}
	if strings.TrimSpace(raw) == "" {
		return f, nil
	}
	var m map[string]map[string]bool
	if err := json.Unmarshal([]byte(raw), &m); err == nil {
		for k, vals := range m {
			for v, on := range vals {
				if on {
					f[k] = append(f[k], v)
				}
			}
		}
		return f, nil
	}
	var l map[string][]string
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		return nil, errInvalid("invalid filters: %s", raw)
	}
	for k, vals := range l {
		f[k] = append(f[k], vals...)
	}
	return f, nil
}

// queryFilters parses the filters= parameter of r.
func queryFilters(r *http.Request) (filters, error) {
	return parseFilters(r.URL.Query().Get("filters"))
}

// has reports whether the key is filtered on.
func (f filters) has(key string) bool { return len(f[key]) > 0 }

// matchLabels reports whether labels satisfy every label filter: "key"
// needs the label, "key=value" needs it with that value. label! negates.
func (f filters) matchLabels(labels map[string]string) bool {
	for _, want := range f["label"] {
		k, v, withValue := strings.Cut(want, "=")
		got, ok := labels[k]
		if !ok || withValue && got != v {
			return false
		}
	}
	for _, not := range f["label!"] {
		k, v, withValue := strings.Cut(not, "=")
		if got, ok := labels[k]; ok && (!withValue || got == v) {
			return false
		}
	}
	return true
}

// matchAny reports whether key is not filtered on, or match accepts one of
// its values: the values of one key are alternatives.
func (f filters) matchAny(key string, match func(string) bool) bool {
	vals := f[key]
	if len(vals) == 0 {
		return true
	}
	for _, v := range vals {
		if match(v) {
			return true
		}
	}
	return false
}
