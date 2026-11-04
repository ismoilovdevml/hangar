// Package builtin lists the formats compiled into the hangar binary.
// Adding a format: create internal/formats/<name>, then add its New() here.
package builtin

import (
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/docker"
	"github.com/ismoilovdevml/hangar/internal/formats/maven"
	"github.com/ismoilovdevml/hangar/internal/formats/npm"
	"github.com/ismoilovdevml/hangar/internal/formats/pypi"
	"github.com/ismoilovdevml/hangar/internal/formats/raw"
)

// Formats returns a new instance of every built-in format.
func Formats() []core.Format {
	return []core.Format{
		docker.New(),
		maven.New(),
		npm.New(),
		pypi.New(),
		raw.New(),
	}
}

// Registry returns a registry with every built-in format.
func Registry() (*formats.Registry, error) {
	return formats.NewRegistry(Formats()...)
}
