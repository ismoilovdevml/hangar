//go:build !webui

package webui

import "io/fs"

func dist() (fs.FS, bool) { return nil, false }
