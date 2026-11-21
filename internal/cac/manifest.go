package cac

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/api/labels"
)

// APIVersion is the apiVersion of every document.
const APIVersion = "hangar/v1"

// Labels hangar apply sets on everything it creates or adopts. The prefix
// is reserved: files may not set labels under it.
const (
	LabelPrefix    = "hangar/"
	LabelManagedBy = LabelPrefix + "managed-by"
	LabelApplySet  = LabelPrefix + "apply-set"
	// ManagedByApply is the value of LabelManagedBy.
	ManagedByApply = "hangar-apply"
)

// maxSecretFile bounds a file read by valueFrom.file.
const maxSecretFile = 1 << 20

// Document is one resource declared in a configuration file.
type Document struct {
	Kind string
	Name string
	// Labels are the labels the file sets (never under LabelPrefix).
	Labels map[string]string
	// Spec is the request body of the resource without its name, in the
	// JSON model (see tree.go), with secret references resolved.
	Spec map[string]any
	// Secrets are the spec paths whose values came from valueFrom.
	Secrets []Path
	// Source is where the document starts, "file:line".
	Source string
}

// ID is "Kind/name".
func (d *Document) ID() string { return d.Kind + "/" + d.Name }

// Resolver resolves valueFrom references. Nil functions use the process
// environment and the file system.
type Resolver struct {
	LookupEnv func(name string) (string, bool)
	ReadFile  func(path string) ([]byte, error)
}

func (r Resolver) lookupEnv(name string) (string, bool) {
	if r.LookupEnv != nil {
		return r.LookupEnv(name)
	}
	return os.LookupEnv(name)
}

func (r Resolver) readFile(path string) ([]byte, error) {
	if r.ReadFile != nil {
		return r.ReadFile(path)
	}
	f, err := os.Open(path) // #nosec G304 -- the path is a file reference the operator wrote
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSecretFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxSecretFile)
	}
	return b, nil
}

// Load reads every document of the given paths: files, directories (their
// .yaml, .yml and .json files; with recursive also those of subdirectories,
// skipping hidden entries) or "-" for stdin. Documents must not repeat a
// kind and name.
func Load(paths []string, recursive bool, stdin io.Reader, r Resolver) ([]*Document, error) {
	var docs []*Document
	seen := map[string]string{}
	add := func(ds []*Document) error {
		for _, d := range ds {
			if prev, ok := seen[d.ID()]; ok {
				return fmt.Errorf("%s: %s is also declared at %s", d.Source, d.ID(), prev)
			}
			seen[d.ID()] = d.Source
			docs = append(docs, d)
		}
		return nil
	}
	for _, p := range paths {
		if p == "-" {
			b, err := io.ReadAll(stdin)
			if err != nil {
				return nil, fmt.Errorf("read stdin: %w", err)
			}
			ds, err := Parse(b, "<stdin>", ".", r)
			if err != nil {
				return nil, err
			}
			if err := add(ds); err != nil {
				return nil, err
			}
			continue
		}
		files, err := manifestFiles(p, recursive)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			b, err := os.ReadFile(f) // #nosec G304 -- a configuration file the operator named
			if err != nil {
				return nil, err
			}
			ds, err := Parse(b, f, filepath.Dir(f), r)
			if err != nil {
				return nil, err
			}
			if err := add(ds); err != nil {
				return nil, err
			}
		}
	}
	return docs, nil
}

func manifestFiles(root string, recursive bool) ([]string, error) {
	st, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return []string{root}, nil
	}
	var out []string
	err = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		if strings.HasPrefix(e.Name(), ".") || (e.IsDir() && !recursive) {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".yaml", ".yml", ".json":
			if !e.IsDir() {
				out = append(out, p)
			}
		}
		return nil
	})
	slices.Sort(out)
	return out, err
}

// Parse reads the documents of one file. baseDir resolves relative
// valueFrom.file paths.
func Parse(data []byte, source, baseDir string, r Resolver) ([]*Document, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []*Document
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", source, err)
		}
		if len(node.Content) == 0 {
			continue // an empty document, e.g. a trailing "---"
		}
		at := fmt.Sprintf("%s:%d", source, node.Content[0].Line)
		var raw any
		if err := node.Decode(&raw); err != nil {
			return nil, fmt.Errorf("%s: %w", at, err)
		}
		if raw == nil {
			continue
		}
		d, err := parseDocument(raw, at, baseDir, r)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", at, err)
		}
		out = append(out, d)
	}
}

func parseDocument(raw any, at, baseDir string, r Resolver) (*Document, error) {
	top, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("a document must be a mapping with apiVersion, kind, metadata and spec")
	}
	if err := onlyKeys(top, "", "apiVersion", "kind", "metadata", "spec"); err != nil {
		return nil, err
	}
	if v, _ := top["apiVersion"].(string); v != APIVersion {
		return nil, fmt.Errorf("apiVersion %q: want %q", top["apiVersion"], APIVersion)
	}
	kindName, _ := top["kind"].(string)
	k := kindByName(kindName)
	if k == nil {
		return nil, fmt.Errorf("kind %q: want one of %s", top["kind"], strings.Join(KindNames(), ", "))
	}
	meta, ok := top["metadata"].(map[string]any)
	if !ok {
		return nil, errors.New("metadata.name is required")
	}
	if err := onlyKeys(meta, "metadata.", "name", "labels"); err != nil {
		return nil, err
	}
	name, _ := meta["name"].(string)
	if name == "" {
		return nil, errors.New("metadata.name is required")
	}
	d := &Document{Kind: k.Name, Name: name, Source: at, Labels: map[string]string{}}
	if l, ok := meta["labels"]; ok && l != nil {
		lm, ok := l.(map[string]any)
		if !ok {
			return nil, errors.New("metadata.labels must be a mapping of strings")
		}
		for key, v := range lm {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("metadata.labels.%s must be a string (quote it)", key)
			}
			if strings.HasPrefix(key, LabelPrefix) {
				return nil, fmt.Errorf("metadata.labels.%s: the %s prefix is reserved for hangar apply", key, LabelPrefix)
			}
			d.Labels[key] = s
		}
		if err := labels.Validate(d.Labels); err != nil {
			return nil, fmt.Errorf("metadata.%w", err)
		}
	}
	spec, ok := top["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("spec is required (the %s settings, as in the REST API)", k.Name)
	}
	resolved, err := r.resolve(spec, nil, baseDir, &d.Secrets)
	if err != nil {
		return nil, err
	}
	j, err := toJSON(resolved)
	if err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	d.Spec = j.(map[string]any)
	if n, ok := d.Spec["name"]; ok {
		if n != name {
			return nil, fmt.Errorf("spec.name %q differs from metadata.name %q (leave spec.name out)", n, name)
		}
		delete(d.Spec, "name")
	}
	if err := onlyKeys(d.Spec, "spec.", k.fields...); err != nil {
		return nil, err
	}
	return d, nil
}

func onlyKeys(m map[string]any, prefix string, allowed ...string) error {
	for _, key := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("unknown field %s%s (want %s)", prefix, key, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// resolve replaces every {valueFrom: {env: NAME}} or {valueFrom: {file:
// PATH}} inside v by the value it names and records the paths.
func (r Resolver) resolve(v any, p Path, baseDir string, secrets *[]Path) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		if ref, ok := x["valueFrom"]; ok {
			if len(x) != 1 {
				return nil, fmt.Errorf("spec.%s: valueFrom must be the only key", p)
			}
			s, err := r.reference(ref, p, baseDir)
			if err != nil {
				return nil, err
			}
			*secrets = append(*secrets, p)
			return s, nil
		}
		out := make(map[string]any, len(x))
		for key, e := range x {
			rv, err := r.resolve(e, p.child(key), baseDir, secrets)
			if err != nil {
				return nil, err
			}
			out[key] = rv
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			// "[]" like mask: a secret anywhere in a list masks every element.
			rv, err := r.resolve(e, p.child("[]"), baseDir, secrets)
			if err != nil {
				return nil, err
			}
			out = append(out, rv)
		}
		return out, nil
	}
	return v, nil
}

func (r Resolver) reference(ref any, p Path, baseDir string) (string, error) {
	m, ok := ref.(map[string]any)
	env, hasEnv := m["env"].(string)
	file, hasFile := m["file"].(string)
	if !ok || len(m) != 1 || hasEnv == hasFile {
		return "", fmt.Errorf("spec.%s: valueFrom needs exactly one of env: NAME or file: PATH", p)
	}
	if hasEnv {
		v, ok := r.lookupEnv(env)
		switch {
		case !ok:
			return "", fmt.Errorf("spec.%s: environment variable %s is not set", p, env)
		case v == "":
			return "", fmt.Errorf("spec.%s: environment variable %s is empty", p, env)
		}
		return v, nil
	}
	if !filepath.IsAbs(file) {
		file = filepath.Join(baseDir, file)
	}
	b, err := r.readFile(file)
	if err != nil {
		return "", fmt.Errorf("spec.%s: %w", p, err)
	}
	// Secret files usually end with a newline that is not part of the value.
	s := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if s == "" {
		return "", fmt.Errorf("spec.%s: %s is empty", p, file)
	}
	return s, nil
}
