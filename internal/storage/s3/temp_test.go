package s3

import (
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

func TestTempStoreFake(t *testing.T) {
	coretest.RunTempStoreTests(t, func(t *testing.T) core.TempStore {
		return newFakeStore(t, newFakeS3(testBucket), nil)
	})
}
