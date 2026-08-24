package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestPUABinaryDependenciesIncludeTimeZoneData(t *testing.T) {
	command := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("list pua dependencies: %v\n%s", err, output)
	}
	for _, dependency := range strings.Fields(string(output)) {
		if dependency == "time/tzdata" {
			return
		}
	}
	t.Fatal("pua dependency graph does not include the embedded time/tzdata package")
}
