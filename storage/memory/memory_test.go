package memory

import (
	"testing"

	"github.com/fpt/go-dquery/storage"
	"github.com/fpt/go-dquery/storage/storagetest"
)

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Store { return NewStore() })
}
