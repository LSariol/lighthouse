package projectstest

import (
	"testing"

	"github.com/LSariol/LightHouse/internal/projects"
)

func TestStore(t *testing.T) {
	RunStoreTests(t, func(t *testing.T) projects.Store { return &Store{} })
}
