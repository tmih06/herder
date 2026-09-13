package health

import (
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/storage"
)

// A nil config must fail the controller section with the load reason.
func TestCheckControllerFailsOnLoadError(t *testing.T) {
	cfg, err := config.Load("/does/not/exist.yaml")
	report := Build(cfg, "/does/not/exist.yaml", err, nil)
	if report.Controller.State != "fail" {
		t.Errorf("controller = %q, want fail", report.Controller.State)
	}
	if !report.OK() {
		return
	}
	t.Error("report with failed controller must not be OK")
}

// A valid config plus a live store must pass the must-work layers.
func TestCheckPassesOnGoodSetup(t *testing.T) {
	cfg, err := config.Load("../../examples/herder.yaml")
	if err != nil {
		t.Fatalf("Load(example) = %v", err)
	}
	store, err := storage.Open(t.TempDir() + "/herder.db")
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	defer store.Close()
	report := Build(cfg, "examples/herder.yaml", nil, store)
	if report.Controller.State != "ok" {
		t.Errorf("controller = %q (%s), want ok", report.Controller.State, report.Controller.Detail)
	}
	if report.Storage.State != "ok" || !strings.Contains(report.Storage.Detail, "herder.db") {
		t.Errorf("storage should name the state file, got %+v", report.Storage)
	}
	if !report.OK() {
		t.Error("good setup must report OK")
	}
}

// Herdr/Docker sections must always be present and labeled, whatever the
// host provides; they warn but never decide OK().
func TestRuntimeSectionsAlwaysLabeled(t *testing.T) {
	report := Build(nil, "x", errTest, nil)
	for _, section := range []Check{report.Herdr, report.Docker} {
		if section.Name != "herdr" && section.Name != "docker" {
			t.Errorf("section mislabeled: %+v", section)
		}
		if section.State == "" || section.Detail == "" {
			t.Errorf("section must carry state and detail: %+v", section)
		}
	}
}

var errTest = errTestType{}

type errTestType struct{}

func (errTestType) Error() string { return "test load failure" }
