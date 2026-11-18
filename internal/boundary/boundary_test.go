package boundary

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The module must stay free of any platform, so any application can use it:
// nothing from D-Bus or golang.org/x/sys, nothing from systemd-whilst, and
// no processes, signals or syslog. Standard packages are checked as direct
// imports only, since os itself depends on syscall.
//
// Every package in this module is checked, test files included, except this
// one: it runs go, so it may use os/exec.
var (
	forbiddenDeps = []string{
		"github.com/godbus/dbus",
		"golang.org/x/sys",
		"github.com/PeterSR/systemd-whilst/",
	}
	forbiddenImports = []string{"os/exec", "syscall", "os/signal", "log/syslog"}
)

type pkg struct {
	ImportPath string
	Imports    []string
}

// goBinary finds the go command: on PATH, or else in the GOROOT this test
// was built with.
func goBinary() (string, bool) {
	if p, err := exec.LookPath("go"); err == nil {
		return p, true
	}
	p := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(p); err == nil {
		return p, true
	}
	return "", false
}

func TestCoreStaysPortable(t *testing.T) {
	gobin, ok := goBinary()
	if !ok {
		t.Skip("cannot find the go command")
	}
	coreDir := filepath.Join("..", "..") // this package is internal/boundary
	list := exec.Command(gobin, "list", "-f", "{{.ImportPath}}", ".")
	list.Dir = coreDir
	out, err := list.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	core := strings.TrimSpace(string(out))
	// base strips the suffixes of external test packages (x_test) and test
	// mains (x.test), which belong to the package they test.
	base := func(p string) string { return strings.TrimSuffix(strings.TrimSuffix(p, ".test"), "_test") }
	inCore := func(p string) bool {
		p = base(p)
		return p == core || strings.HasPrefix(p, core+"/")
	}
	self := core + "/internal/boundary"

	cmd := exec.Command(gobin, "list", "-deps", "-test", "-json=ImportPath,Imports", "./...")
	cmd.Dir = coreDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	seen := 0
	for {
		var p pkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		seen++
		path, _, _ := strings.Cut(p.ImportPath, " ") // "x [x.test]" for test variants
		if !inCore(path) {
			for _, f := range forbiddenDeps {
				if strings.HasPrefix(path, f) {
					t.Errorf("the core depends on %s", path)
				}
			}
			continue
		}
		if base(path) == self {
			continue
		}
		for _, imp := range p.Imports {
			for _, f := range forbiddenImports {
				if imp == f {
					t.Errorf("%s imports %s", p.ImportPath, imp)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("go list returned no packages")
	}
}
