package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// binPath is the plugin binary, built once in TestMain so plugin.Main's
// version/bootstrap subcommands can be exercised as a real black-box CLI
// rather than only unit-tested inside the library.
var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fleeting-plugin-openstack-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating temp dir:", err)
		os.Exit(1)
	}

	binPath = filepath.Join(dir, "fleeting-plugin-openstack")
	if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building plugin binary: %v\n%s", err, out)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestVersionSubcommandPrintsBuildMetadata(t *testing.T) {
	out, err := exec.Command(binPath, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("running %q version: %v\n%s", binPath, err, out)
	}

	if !strings.Contains(string(out), "fleeting-plugin-openstack") {
		t.Errorf("version output = %q, want it to contain the plugin name", out)
	}
	if !strings.Contains(string(out), "Version:") {
		t.Errorf("version output = %q, want a Version: line", out)
	}
}

func TestVersionFlagPrintsBuildMetadata(t *testing.T) {
	out, err := exec.Command(binPath, "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("running %q -version: %v\n%s", binPath, err, out)
	}

	if !strings.Contains(string(out), "Version:") {
		t.Errorf("-version output = %q, want a Version: line", out)
	}
}

func TestBootstrapWithoutRepoArgFails(t *testing.T) {
	out, err := exec.Command(binPath, "bootstrap").CombinedOutput()
	if err == nil {
		t.Fatalf("running %q bootstrap with no repo arg: expected non-zero exit, got success: %s", binPath, out)
	}

	if !strings.Contains(string(out), "bootstrap requires plugin repository path") {
		t.Errorf("bootstrap error output = %q, want it to explain the missing repo argument", out)
	}
}
