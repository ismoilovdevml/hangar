package cac

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ExportOptions select what Export writes.
type ExportOptions struct {
	// Kinds are the document kinds to export; empty means all.
	Kinds []string
	// ApplySet, when set, limits the export to resources it owns.
	ApplySet string
}

// secretRef is a secret Export replaced by a reference.
type secretRef struct {
	id   string // Kind/name
	path Path
	env  string
}

// harmlessServiceIndexes are NuGet service index paths that carry no feed
// key; any other value is exported as a reference.
var harmlessServiceIndexes = []string{"v3/index.json", "index.json", "api/v3/index.json"}

// Export writes the server's configuration as documents that apply
// reproduces. Secrets are never written: every secret the server holds
// becomes a valueFrom: {env: NAME} reference, and the variables are listed
// at the top. Built-in roles and resources defined by the server
// configuration file are left out. It returns notes about what was left out.
func Export(ctx context.Context, d Doer, o ExportOptions, w io.Writer) ([]string, error) {
	for _, name := range o.Kinds {
		if kindByName(name) == nil {
			return nil, fmt.Errorf("kind %q: want one of %s", name, strings.Join(KindNames(), ", "))
		}
	}
	if o.ApplySet != "" {
		if err := checkApplySet(o.ApplySet); err != nil {
			return nil, err
		}
	}
	var notes []string
	var refs []secretRef
	var docs []*yaml.Node
	for _, k := range kinds {
		if len(o.Kinds) > 0 && !slices.Contains(o.Kinds, k.Name) {
			continue
		}
		ls, err := fetchLive(ctx, d, k)
		if err != nil {
			return nil, err
		}
		var cs []*Change
		for name, obj := range ls.objects {
			if why := protected(k, obj); why != "" {
				notes = append(notes, fmt.Sprintf("%s/%s left out: %s", k.Name, name, why))
				continue
			}
			if o.ApplySet != "" && !ownedBy(ls.labels[name], o.ApplySet) {
				continue
			}
			c := &Change{Kind: k.Name, Name: name, k: k, body: obj}
			if members, ok := getPath(obj, Path{"config", "group", "members"}); ok {
				c.group = stringList(members)
			}
			cs = append(cs, c)
		}
		cs, err = order(cs)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			spec := k.project(c.body)
			for _, r := range exportSecrets(k, c.Name, c.body, spec) {
				setPath(spec, r.path, map[string]any{"valueFrom": map[string]any{"env": r.env}})
				refs = append(refs, r)
			}
			node, err := documentNode(k.Name, c.Name, userLabels(ls.labels[c.Name]), spec)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", c.ID(), err)
			}
			docs = append(docs, node)
		}
	}
	if err := writeHeader(w, refs); err != nil {
		return nil, err
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	for _, n := range docs {
		if err := enc.Encode(n); err != nil {
			return nil, err
		}
	}
	return notes, enc.Close()
}

// exportSecrets lists the secrets of one object that the server holds but
// does not return, and the sensitive values it does return.
func exportSecrets(k *kind, name string, obj, spec map[string]any) []secretRef {
	ref := func(p Path) secretRef {
		tail := p[max(0, len(p)-2):]
		env := "HANGAR_SECRET_" + envName(k.LabelKind) + "_" + envName(name) + "_" + envName(strings.Join(tail, "_"))
		return secretRef{id: k.Name + "/" + name, path: p, env: env}
	}
	var out []secretRef
	switch k.Name {
	case KindRepository:
		if auth, ok := getPath(spec, Path{"config", "proxy", "auth"}); ok {
			if a, _ := auth.(map[string]any); a != nil {
				if a["kind"] == "basic" {
					out = append(out, ref(Path{"config", "proxy", "auth", "password"}))
				} else {
					out = append(out, ref(Path{"config", "proxy", "auth", "token"}))
				}
			}
		}
		for _, p := range k.urlSecrets {
			if v, ok := getPath(spec, p); ok {
				if s, _ := v.(string); urlHasPassword(s) {
					out = append(out, ref(p))
				}
			}
		}
		for _, p := range k.sensitive {
			if v, ok := getPath(spec, p); ok {
				if s, _ := v.(string); s != "" && !slices.Contains(harmlessServiceIndexes, s) {
					out = append(out, ref(p))
				}
			}
		}
	case KindBlobStore:
		if id, ok := getPath(spec, Path{"s3", "access_key_id"}); ok && !isZero(id) {
			out = append(out, ref(Path{"s3", "secret_access_key"}))
		}
	case KindOutboundProxy:
		if obj["password_configured"] == true {
			out = append(out, ref(Path{"password"}))
		}
	}
	return out
}

// envName turns s into the upper-case letters, digits and underscores of
// an environment variable name.
func envName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, s)
}

// userLabels drops the labels apply manages itself.
func userLabels(l map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range l {
		if !strings.HasPrefix(k, LabelPrefix) {
			out[k] = v
		}
	}
	return out
}

func writeHeader(w io.Writer, refs []secretRef) error {
	var b strings.Builder
	b.WriteString("# Exported by hangar export. Apply with: hangar apply -f <this file>\n")
	if len(refs) > 0 {
		b.WriteString("# Secrets are never exported. Set these environment variables (or edit the\n")
		b.WriteString("# references to valueFrom: {file: PATH}) before applying:\n")
		for _, r := range refs {
			fmt.Fprintf(&b, "#   %s  (%s spec.%s)\n", r.env, r.id, r.path)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// documentNode builds a document with its keys in the conventional order.
func documentNode(kindName, name string, labels map[string]string, spec map[string]any) (*yaml.Node, error) {
	meta := map[string]any{"name": name}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	doc := &yaml.Node{Kind: yaml.MappingNode}
	for _, kv := range []struct {
		key   string
		value any
	}{{"apiVersion", APIVersion}, {"kind", kindName}, {"metadata", meta}, {"spec", plain(spec)}} {
		var v yaml.Node
		if err := v.Encode(kv.value); err != nil {
			return nil, err
		}
		doc.Content = append(doc.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: kv.key}, &v)
	}
	// "name" first in metadata reads better than alphabetical order.
	if m := doc.Content[5]; len(m.Content) == 4 && m.Content[0].Value == "labels" {
		m.Content = []*yaml.Node{m.Content[2], m.Content[3], m.Content[0], m.Content[1]}
	}
	return doc, nil
}

// plain converts the JSON model into values YAML writes naturally: numbers
// as integers where they are.
func plain(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for _, k := range slices.Sorted(maps.Keys(x)) {
			out[k] = plain(x[k])
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, plain(e))
		}
		return out
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return string(x)
	}
	return v
}
