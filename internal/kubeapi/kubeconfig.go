package kubeapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is a kubeconfig, as far as the app reads one: its clusters, users
// and contexts, by the names kubeconfig files give their fields. It reads
// the same from YAML, as k3s writes the file, and from JSON, as
// kubectl config view -o json prints it.
type Config struct {
	Clusters []struct {
		Name    string `json:"name"`
		Cluster struct {
			Server     string `json:"server"`
			CA         string `json:"certificate-authority-data"`
			ServerName string `json:"tls-server-name,omitempty"`
		} `json:"cluster"`
	} `json:"clusters"`
	Users []struct {
		Name string `json:"name"`
		User struct {
			Cert  string `json:"client-certificate-data"`
			Key   string `json:"client-key-data"`
			Token string `json:"token"`
		} `json:"user"`
	} `json:"users"`
	Contexts []struct {
		Name    string `json:"name"`
		Context struct {
			Cluster string `json:"cluster"`
			User    string `json:"user"`
		} `json:"context"`
	} `json:"contexts"`
	Current string `json:"current-context"`
}

// Entry is a context with its cluster and user, as one: where the API
// server is, the CA it is checked against, and the credentials, the
// certificate data in base64 as the file has it.
type Entry struct {
	Server, CA, Cert, Key, Token string
	// ServerName is the name the server's certificate is checked for, when
	// it is not the server's host.
	ServerName string
}

// Entry is the context named name, or the current one for "".
func (k Config) Entry(name string) (Entry, bool) {
	if name == "" {
		name = k.Current
	}
	for _, c := range k.Contexts {
		if c.Name != name {
			continue
		}
		var e Entry
		for _, cl := range k.Clusters {
			if cl.Name == c.Context.Cluster {
				e.Server, e.CA, e.ServerName = cl.Cluster.Server, cl.Cluster.CA, cl.Cluster.ServerName
			}
		}
		for _, u := range k.Users {
			if u.Name == c.Context.User {
				e.Cert, e.Key, e.Token = u.User.Cert, u.User.Key, u.User.Token
			}
		}
		return e, true
	}
	return Entry{}, false
}

// ParseConfig reads a kubeconfig in JSON or in YAML. Of YAML it reads the
// form client-go writes, which is the form of the file k3s writes and of
// one kubectl config writes: block mappings and sequences of plain or
// quoted scalars on one line each, with {} and [] for empty ones, and
// comments. It refuses what it does not read rather than misread it:
// flow collections with content, block scalars (| and >), anchors,
// aliases, tags and documents past the first.
func ParseConfig(data []byte) (Config, error) {
	var k Config
	text := strings.TrimSpace(string(data))
	if strings.HasPrefix(text, "{") {
		err := json.Unmarshal(data, &k)
		return k, err
	}
	tree, err := parseYAML(string(data))
	if err != nil {
		return k, fmt.Errorf("kubeconfig: %w", err)
	}
	if tree == nil {
		return k, nil
	}
	if _, ok := tree.(map[string]any); !ok {
		return k, errors.New("kubeconfig: not a mapping")
	}
	b, err := json.Marshal(tree)
	if err != nil {
		return k, err
	}
	if err := json.Unmarshal(b, &k); err != nil {
		return k, fmt.Errorf("kubeconfig: %w", err)
	}
	return k, nil
}

// ReadConfig reads the kubeconfig file at path.
func ReadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return ParseConfig(b)
}

// ---- A YAML reader for kubeconfig files ---------------------------------

// yamlLine is a line of YAML with what it is indented by, its comment and
// trailing space taken off.
type yamlLine struct {
	n      int // its number, from 1
	indent int
	text   string
}

// parseYAML reads the YAML subset ParseConfig describes into maps of
// strings to values, slices of values, strings and nils.
func parseYAML(src string) (any, error) {
	var lines []yamlLine
	for i, raw := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		body := strings.TrimLeft(raw, " ")
		if strings.HasPrefix(body, "\t") {
			return nil, fmt.Errorf("line %d: indented with a tab", i+1)
		}
		body = strings.TrimRight(stripComment(body), " \t")
		if body == "" {
			continue
		}
		if body == "---" && len(lines) == 0 {
			continue // the start of the first document
		}
		if body == "---" || body == "..." {
			break // a document after the first is not read
		}
		lines = append(lines, yamlLine{n: i + 1, indent: len(raw) - len(strings.TrimLeft(raw, " ")), text: body})
	}
	if len(lines) == 0 {
		return nil, nil
	}
	p := &yamlParser{lines: lines}
	v, err := p.node(lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.at < len(p.lines) {
		return nil, fmt.Errorf("line %d: unexpected indentation", p.lines[p.at].n)
	}
	return v, nil
}

// stripComment drops a comment from a line: a # at its start or after a
// space, outside quotes.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			if i == 0 || s[i-1] == ' ' || s[i-1] == ':' || s[i-1] == '-' {
				quote = c
			}
		case c == '#' && (i == 0 || s[i-1] == ' '):
			return s[:i]
		}
	}
	return s
}

type yamlParser struct {
	lines []yamlLine
	at    int
}

// node reads the mapping or sequence whose lines are indented by indent.
func (p *yamlParser) node(indent int) (any, error) {
	l := p.lines[p.at]
	if l.indent != indent {
		return nil, fmt.Errorf("line %d: unexpected indentation", l.n)
	}
	if isItem(l.text) {
		return p.sequence(indent)
	}
	if _, _, ok := splitKey(l.text); !ok {
		// A document that is a scalar alone.
		p.at++
		return scalar(l.text, l.n)
	}
	return p.mapping(indent)
}

func isItem(text string) bool { return text == "-" || strings.HasPrefix(text, "- ") }

// sequence reads the items at indent.
func (p *yamlParser) sequence(indent int) ([]any, error) {
	out := []any{}
	for p.at < len(p.lines) {
		l := p.lines[p.at]
		if l.indent < indent {
			break
		}
		if l.indent > indent || !isItem(l.text) {
			if l.indent == indent {
				break // the sequence was a mapping's value at its key's indent
			}
			return nil, fmt.Errorf("line %d: unexpected indentation", l.n)
		}
		rest := strings.TrimLeft(strings.TrimPrefix(l.text, "-"), " ")
		if rest == "" {
			// The item is on the lines under the dash.
			p.at++
			if p.at < len(p.lines) && p.lines[p.at].indent > indent {
				v, err := p.node(p.lines[p.at].indent)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			} else {
				out = append(out, nil)
			}
			continue
		}
		if _, _, ok := splitKey(rest); ok || isItem(rest) {
			// "- key: value": a mapping (or sequence) that starts on the
			// dash's line, at the column after the dash. Read it as if it
			// were on a line of its own.
			col := indent + (len(l.text) - len(rest))
			p.lines[p.at] = yamlLine{n: l.n, indent: col, text: rest}
			v, err := p.node(col)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			continue
		}
		p.at++
		v, err := scalar(rest, l.n)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// mapping reads the keys at indent and their values.
func (p *yamlParser) mapping(indent int) (map[string]any, error) {
	out := map[string]any{}
	for p.at < len(p.lines) {
		l := p.lines[p.at]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation", l.n)
		}
		if isItem(l.text) {
			break
		}
		key, rest, ok := splitKey(l.text)
		if !ok {
			return nil, fmt.Errorf("line %d: expected a key", l.n)
		}
		k, err := scalar(key, l.n)
		if err != nil {
			return nil, err
		}
		name, _ := k.(string)
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("line %d: %q is given twice", l.n, name)
		}
		p.at++
		if rest != "" {
			if out[name], err = scalar(rest, l.n); err != nil {
				return nil, err
			}
			continue
		}
		// The value is on the lines under the key: further in, or a
		// sequence at the key's own indent, as client-go writes them.
		if p.at < len(p.lines) {
			next := p.lines[p.at]
			if next.indent > indent || next.indent == indent && isItem(next.text) {
				if out[name], err = p.node(next.indent); err != nil {
					return nil, err
				}
				continue
			}
		}
		out[name] = nil
	}
	return out, nil
}

// splitKey splits "key: value" or "key:" into the key and the value.
func splitKey(text string) (key, value string, ok bool) {
	if text == "" {
		return "", "", false
	}
	if q := text[0]; q == '"' || q == '\'' {
		end := closingQuote(text)
		if end < 0 || end+1 >= len(text) || text[end+1] != ':' {
			return "", "", false
		}
		after := text[end+2:]
		if after != "" && after[0] != ' ' {
			return "", "", false
		}
		return text[:end+1], strings.TrimSpace(after), true
	}
	for i := 0; i < len(text); i++ {
		if text[i] == ':' && (i+1 == len(text) || text[i+1] == ' ') {
			return text[:i], strings.TrimSpace(text[i+1:]), true
		}
	}
	return "", "", false
}

// closingQuote is the index of the quote that closes the one text starts
// with, or -1.
func closingQuote(text string) int {
	q := text[0]
	for i := 1; i < len(text); i++ {
		switch {
		case q == '"' && text[i] == '\\':
			i++
		case q == '\'' && text[i] == '\'' && i+1 < len(text) && text[i+1] == '\'':
			i++ // an escaped quote
		case text[i] == q:
			return i
		}
	}
	return -1
}

// scalar reads a value written on one line.
func scalar(text string, n int) (any, error) {
	switch text {
	case "{}":
		return map[string]any{}, nil
	case "[]":
		return []any{}, nil
	case "~", "null", "Null", "NULL":
		return nil, nil
	}
	switch text[0] {
	case '"':
		if closingQuote(text) != len(text)-1 {
			return nil, fmt.Errorf("line %d: unterminated string", n)
		}
		s, err := strconv.Unquote(text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", n, err)
		}
		return s, nil
	case '\'':
		if closingQuote(text) != len(text)-1 {
			return nil, fmt.Errorf("line %d: unterminated string", n)
		}
		return strings.ReplaceAll(text[1:len(text)-1], "''", "'"), nil
	case '{', '[':
		return nil, fmt.Errorf("line %d: flow collections are not read", n)
	case '|', '>':
		return nil, fmt.Errorf("line %d: block scalars are not read", n)
	case '&', '*', '!':
		return nil, fmt.Errorf("line %d: anchors, aliases and tags are not read", n)
	}
	return text, nil
}
