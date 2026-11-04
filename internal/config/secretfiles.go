package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// ApplySecretsDir sets settings from the files in dir: a file named after a
// setting's YAML path (security.secret_key, database.url,
// auth.oidc.client_secret) holds its value; trailing line breaks are
// dropped. Names starting with "." (Kubernetes secret volume internals) and
// names without a dot (helper files such as postgres-password) are skipped.
// A dotted name that is not a setting, an empty file or an unreadable file
// is an error, so a typo never passes silently. An empty dir does nothing.
func ApplySecretsDir(cfg *Config, dir string) error {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("secrets_dir: %w", err)
	}
	fields := map[string]reflect.Value{}
	walkPath(reflect.ValueOf(cfg).Elem(), nil, func(path []string, v reflect.Value) {
		fields[strings.Join(path, ".")] = v
	})
	delete(fields, "secrets_dir")
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.Contains(name, ".") {
			continue
		}
		p := filepath.Join(dir, name)
		// Follow symlinks (Kubernetes projects keys as links into ..data).
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			continue
		}
		v, ok := fields[name]
		if !ok {
			errs = append(errs, fmt.Errorf("secrets_dir: %s is not a setting (want a YAML path such as security.secret_key)", name))
			continue
		}
		raw, err := readSecretFile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("secrets_dir: %w", err))
			continue
		}
		if err := setValue(v, raw); err != nil {
			errs = append(errs, fmt.Errorf("secrets_dir: %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// readSecretFile returns the file's contents without trailing line breaks;
// an empty value is an error.
func readSecretFile(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied secret path
	if err != nil {
		return "", err
	}
	v := strings.TrimRight(string(b), "\r\n")
	if v == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return v, nil
}
