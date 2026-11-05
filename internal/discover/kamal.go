// Package discover reads a repository statically to propose deployment units: Kamal
// configs, destination overlays, wrapper scripts, build contexts, secrets files, hosts and
// CI workflows. It never evaluates ERB, never runs project scripts and never reads secret
// values; anything it cannot determine is reported as unknown.
package discover

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const dynamicMarker = "__KAMAL_AUTODEPLOY_DYNAMIC__"

var erbPattern = regexp.MustCompile(`(?s)<%.*?%>`)

const maxFile = 256 << 10

// readFile reads a regular, non-symlink file within the size limit.
func readFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() > maxFile {
		return nil, errors.New("file exceeds the discovery size limit")
	}
	return os.ReadFile(path)
}

// kamalFile is one parsed Kamal YAML file. ERB is replaced by a marker before parsing, so
// a templated value reads as unknown, never as a guess.
type kamalFile struct {
	Path    string // repository-relative
	Raw     map[string]any
	Dynamic bool
}

func parseKamal(root, rel string) (*kamalFile, error) {
	raw, err := readFile(filepath.Join(root, rel))
	if err != nil {
		return nil, err
	}
	dynamic := bytes.Contains(raw, []byte("<%"))
	raw = erbPattern.ReplaceAll(raw, []byte(dynamicMarker))
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, errors.New(rel + " cannot be parsed as static YAML")
	}
	if m == nil {
		m = map[string]any{}
	}
	return &kamalFile{Path: rel, Raw: m, Dynamic: dynamic}, nil
}

func str(v any) string {
	switch s := v.(type) {
	case string:
		if strings.Contains(s, dynamicMarker) {
			return ""
		}
		return s
	case int:
		return strconv.Itoa(s)
	}
	return ""
}

func dynamic(v any) bool {
	s, ok := v.(string)
	return ok && strings.Contains(s, dynamicMarker)
}

func mapping(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

// merge overlays b onto a (Kamal destination files deep-merge maps).
func merge(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if bv, ok := v.(map[string]any); ok {
			if av, ok := out[k].(map[string]any); ok {
				v = merge(av, bv)
			}
		}
		out[k] = v
	}
	return out
}

// kamalConfig is one deployable Kamal configuration: a base file plus optional destination.
type kamalConfig struct {
	Base        string // repository-relative base config
	Overlay     string // repository-relative destination overlay, "" for none
	Destination string
	Merged      map[string]any
	Dynamic     bool
}

func (k kamalConfig) service() string { return str(k.Merged["service"]) }

// hosts lists static hosts across roles; unknown is true when any role's hosts are dynamic.
func (k kamalConfig) hosts() (hosts []string, unknown bool) {
	seen := map[string]bool{}
	add := func(h string) {
		if h != "" && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	servers := k.Merged["servers"]
	var roles map[string]any
	switch s := servers.(type) {
	case []any:
		roles = map[string]any{"web": s}
	case map[string]any:
		roles = s
	default:
		return nil, true
	}
	for _, role := range sortedKeys(roles) {
		v := roles[role]
		if m := mapping(v); m != nil {
			v = m["hosts"]
		}
		entries := list(v)
		if entries == nil {
			unknown = true
			continue
		}
		for _, e := range entries {
			switch h := e.(type) {
			case string:
				if dynamic(h) {
					unknown = true
				} else {
					add(h)
				}
			case map[string]any: // - 203.0.113.10: [tag]
				for key := range h {
					if dynamic(key) {
						unknown = true
					} else {
						add(key)
					}
				}
			default:
				unknown = true
			}
		}
	}
	sort.Strings(hosts)
	return hosts, unknown
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// secretNames is every secret the deploy needs: env.secret (also per role), registry
// password and builder secrets. Accessory secrets are listed separately: deploy and
// rollback never boot accessories.
func (k kamalConfig) secretNames() (deploy, accessory []string, unknown bool) {
	add := func(dst *[]string, v any) {
		for _, e := range list(v) {
			s := str(e)
			if s == "" {
				unknown = true
				continue
			}
			// ALIAS:SECRET reads SECRET from the secrets file into ALIAS.
			if _, secret, ok := strings.Cut(s, ":"); ok {
				s = secret
			}
			*dst = appendUnique(*dst, s)
		}
	}
	add(&deploy, mapping(k.Merged["env"])["secret"])
	if roles := mapping(k.Merged["servers"]); roles != nil {
		for _, r := range sortedKeys(roles) {
			add(&deploy, mapping(mapping(roles[r])["env"])["secret"])
		}
	}
	registry := mapping(k.Merged["registry"])
	add(&deploy, registry["password"])
	add(&deploy, registry["username"])
	add(&deploy, mapping(k.Merged["builder"])["secrets"])
	for _, a := range sortedKeys(mapping(k.Merged["accessories"])) {
		add(&accessory, mapping(mapping(mapping(k.Merged["accessories"])[a])["env"])["secret"])
	}
	// registry.username is usually a literal, not a secret list; only list entries count.
	sort.Strings(deploy)
	sort.Strings(accessory)
	return deploy, accessory, unknown
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// findKamalConfigs lists base configs and their destination overlays. A deploy.X.yml
// next to deploy.yml is an overlay unless it declares a different service; without a
// deploy.yml, every deploy*.yml is a standalone config (the example application's deploy.rails.yml).
func findKamalConfigs(root string) ([]kamalConfig, []Note) {
	var notes []Note
	var out []kamalConfig
	for _, dir := range []string{"config", "."} {
		matches, _ := filepath.Glob(filepath.Join(root, dir, "deploy*.yml"))
		sort.Strings(matches)
		if len(matches) == 0 {
			continue
		}
		files := map[string]*kamalFile{}
		for _, m := range matches {
			rel, _ := filepath.Rel(root, m)
			rel = filepath.ToSlash(rel)
			f, err := parseKamal(root, rel)
			if err != nil {
				notes = append(notes, Note{Level: Unknown, Text: rel + ": " + err.Error()})
				continue
			}
			files[rel] = f
		}
		baseRel := filepath.ToSlash(filepath.Join(dir, "deploy.yml"))
		base := files[baseRel]
		baseService := ""
		if base != nil {
			baseService = str(base.Raw["service"])
		}
		var overlays []string
		for _, rel := range sortedFileKeys(files) {
			if rel == baseRel {
				continue
			}
			f := files[rel]
			svc := str(f.Raw["service"])
			if base != nil && (svc == "" || svc == baseService) && filepath.Base(rel) != "deploy.yml" {
				overlays = append(overlays, rel)
				continue
			}
			if svc == "" && base == nil && !hasKey(f.Raw, "service") {
				notes = append(notes, Note{Level: Unknown, Text: rel + " declares no service and there is no deploy.yml to overlay; skipped"})
				continue
			}
			out = append(out, kamalConfig{Base: rel, Merged: f.Raw, Dynamic: f.Dynamic})
		}
		if base != nil {
			if len(overlays) == 0 {
				out = append([]kamalConfig{{Base: baseRel, Destination: "", Merged: base.Raw, Dynamic: base.Dynamic}}, out...)
			}
			for _, rel := range overlays {
				dest := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(rel), "deploy."), ".yml")
				f := files[rel]
				out = append(out, kamalConfig{Base: baseRel, Overlay: rel, Destination: dest, Merged: merge(base.Raw, f.Raw), Dynamic: base.Dynamic || f.Dynamic})
			}
		}
		break // config/ wins over the repository root, like Kamal's default
	}
	return out, notes
}

func hasKey(m map[string]any, k string) bool { _, ok := m[k]; return ok }

func sortedFileKeys(m map[string]*kamalFile) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
