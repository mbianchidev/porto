package setup

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mbianchidev/porto/internal/app"
)

func TestRecordedComposeBuildUsesTheSelectedDefinitionAndExistingProjectIdentity(t *testing.T) {
	project := app.Project{ID: 37, Name: "synthetic", Path: t.TempDir(), Strategy: "compose", ComposeFile: filepath.Join(t.TempDir(), "37.json")}
	commands, err := Plan(project)
	if err != nil {
		t.Fatal(err)
	}
	expected := []Command{{Name: "docker", Args: []string{"compose", "--project-directory", project.Path, "-p", "porto-37", "-f", project.ComposeFile, "build", "--no-cache"}}}
	if !reflect.DeepEqual(commands, expected) {
		t.Fatalf("build did not preserve the recorded definition and ownership: %+v", commands)
	}
}
