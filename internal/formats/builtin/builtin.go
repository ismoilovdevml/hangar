// Package builtin lists the formats compiled into the hangar binary.
// Adding a format: create internal/formats/<name>, then add its New() here.
package builtin

import (
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/docker"
	"github.com/ismoilovdevml/hangar/internal/formats/gomod"
	"github.com/ismoilovdevml/hangar/internal/formats/helm"
	"github.com/ismoilovdevml/hangar/internal/formats/maven"
	"github.com/ismoilovdevml/hangar/internal/formats/npm"
	"github.com/ismoilovdevml/hangar/internal/formats/nuget"
	"github.com/ismoilovdevml/hangar/internal/formats/pypi"
	"github.com/ismoilovdevml/hangar/internal/formats/raw"
)

// Options carries server configuration for the formats that take any.
type Options struct {
	Docker docker.Options
}

// Formats returns a new instance of every built-in format with default options.
func Formats() []core.Format { return FormatsWith(Options{}) }

// FormatsWith returns a new instance of every built-in format configured by o.
func FormatsWith(o Options) []core.Format {
	return []core.Format{
		docker.NewWithOptions(o.Docker),
		gomod.New(),
		helm.New(),
		maven.New(),
		npm.New(),
		nuget.New(),
		pypi.New(),
		raw.New(),
	}
}

// Registry returns a registry with every built-in format.
func Registry() (*formats.Registry, error) { return RegistryWith(Options{}) }

// RegistryWith returns a registry with every built-in format configured by o.
func RegistryWith(o Options) (*formats.Registry, error) {
	return formats.NewRegistry(FormatsWith(o)...)
}
