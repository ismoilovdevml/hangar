package coretest_test

import (
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

func TestMemTempStore(t *testing.T) {
	coretest.RunTempStoreTests(t, func(*testing.T) core.TempStore { return coretest.NewMemBlobStore() })
}
