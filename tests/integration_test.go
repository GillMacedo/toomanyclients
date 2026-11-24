// Package tests builds toomanyclients and runs it on the examples, on real
// code, and on broken copies of each input.
package tests

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "toomanyclients-test")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "toomanyclients")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "./plugin/scripts")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func runBin(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errb.String()
}

func TestExamples(t *testing.T) {
	code, out, _ := runBin(t, "..", "examples/oversubscribed")
	if code != 1 || !strings.Contains(out, "ERROR TC101") || !strings.Contains(out, "at least 168") {
		t.Fatalf("oversubscribed: %d\n%s", code, out)
	}
	code, out, _ = runBin(t, "..", "examples/healthy")
	if code != 0 || !strings.Contains(out, "Result: at most 70 connections at full scale and 90 during a rolling update, within the 97") {
		t.Fatalf("healthy: %d\n%s", code, out)
	}
	code, out, errs := runBin(t, "..", "examples/broken", "examples/healthy/cmd", "examples/healthy/internal")
	if code != 3 || out != "" || !strings.Contains(errs, "failed command") {
		t.Fatalf("broken: %d\n%s%s", code, out, errs)
	}
}

// TestRealCode runs on files from evrone/go-clean-template, where the pool
// size goes through an env struct tag, a config struct, a functional
// option and a conversion helper.
func TestRealCode(t *testing.T) {
	code, out, _ := runBin(t, "..", "--max-connections", "100", "tests/testdata/go-clean-template")
	if code != 0 || !strings.Contains(out, "MaxConns 2") || !strings.Contains(out, "Result: at most 6 connections at full scale and 8 during a rolling update") {
		t.Fatalf("%d\n%s", code, out)
	}
	// Without PG_POOL_MAX the program refuses to start (required tag), so
	// the size is unknown rather than the struct's default of 1.
	dir := copyTree(t, "testdata/go-clean-template")
	yaml := filepath.Join(dir, "deploy", "app.yaml")
	data, _ := os.ReadFile(yaml)
	os.WriteFile(yaml, []byte(strings.Replace(string(data), "          env:\n            - name: PG_POOL_MAX\n              value: \"2\"\n", "", 1)), 0o644)
	code, out, _ = runBin(t, dir, "--max-connections", "100", ".")
	if code != 1 || !strings.Contains(out, "TC901") || !strings.Contains(out, "requires") {
		t.Fatalf("%d\n%s", code, out)
	}
}

func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// brokenContents are inputs that must never give a result: empty, random
// bytes, random text, and the output of failed builds and runs.
func brokenContents() map[string][]byte {
	r := rand.New(rand.NewSource(1))
	random := make([]byte, 4096)
	r.Read(random)
	return map[string][]byte{
		"empty":         {},
		"random bytes":  random,
		"random text":   []byte("lorem ipsum dolor sit amet, consectetur adipiscing elit\nsed do eiusmod tempor\n"),
		"go build":      []byte("# example.com/shop/oversubscribed/internal/store\ninternal/store/store.go:24:13: undefined: sql.Opn\n"),
		"go test":       []byte("--- FAIL: TestOpen (0.00s)\n    store_test.go:10: boom\nFAIL\nFAIL\texample.com/shop\t0.01s\n"),
		"panic":         []byte("panic: runtime error: invalid memory address or nil pointer dereference\n[signal SIGSEGV]\n\ngoroutine 1 [running]:\nmain.main()\n"),
		"helm error":    []byte("Error: INSTALLATION FAILED: template: shop/templates/api.yaml:12:3: executing \"shop/templates/api.yaml\" at <.Values.x>: nil pointer\n"),
		"kubectl error": []byte("error: unable to recognize \"deploy/api.yaml\": no matches for kind \"Deployment\"\n"),
		"psql error":    []byte("psql: error: connection to server on socket \"/tmp/.s.PGSQL.5432\" failed: FATAL:  sorry, too many clients already\n"),
	}
}

// TestBrokenFilesEachSide replaces the Go source, then a manifest, then the
// Postgres server object, then a --pg-config file, with each broken
// content while the other inputs stay real. Each must exit 3 with nothing
// on stdout, also with --fail-on never.
func TestBrokenFilesEachSide(t *testing.T) {
	for _, target := range []string{"internal/store/store.go", "cmd/worker/main.go", "deploy/api.yaml", "deploy/worker.yaml", "deploy/postgres.yaml"} {
		for name, content := range brokenContents() {
			dir := copyTree(t, "../examples/oversubscribed")
			if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(target)), content, 0o644); err != nil {
				t.Fatal(err)
			}
			// A cut-off copy of the same file.
			for _, fail := range []string{"warn", "never"} {
				code, out, errs := runBin(t, dir, "--fail-on", fail, ".")
				if code != 3 || out != "" || !strings.Contains(errs, "no result") {
					t.Errorf("%s as %s (--fail-on %s): exit %d\nstdout: %s\nstderr: %s", name, target, fail, code, out, errs)
				}
			}
		}
	}
	for name, content := range brokenContents() {
		dir := copyTree(t, "../examples/healthy")
		os.Remove(filepath.Join(dir, "deploy", "postgres.yaml"))
		conf := filepath.Join(t.TempDir(), "postgresql.conf")
		os.WriteFile(conf, content, 0o644)
		for _, fail := range []string{"warn", "never"} {
			code, out, errs := runBin(t, dir, "--fail-on", fail, "--pg-config", conf, ".")
			if code != 3 || out != "" {
				t.Errorf("%s as postgresql.conf (--fail-on %s): exit %d\n%s%s", name, fail, code, out, errs)
			}
		}
	}
}

// TestEveryCut cuts each file of the oversubscribed example after every
// line and every 37th byte, with the other files intact. The full example
// has a finding, so no cut may print a clean result or exit 0.
func TestEveryCut(t *testing.T) {
	src := "../examples/oversubscribed"
	var files []string
	filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(src, p)
			files = append(files, rel)
		}
		return nil
	})
	base := copyTree(t, src)
	runs := 0
	for _, rel := range files {
		full, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatal(err)
		}
		var cuts []int
		for i, b := range full {
			if b == '\n' && i+1 < len(full) {
				cuts = append(cuts, i+1)
			}
		}
		cuts = append(cuts, 0)
		for i := 1; i < len(full); i += 37 {
			cuts = append(cuts, i)
		}
		target := filepath.Join(base, rel)
		for _, n := range cuts {
			os.WriteFile(target, full[:n], 0o644)
			code, out, _ := runBin(t, base, ".")
			runs++
			if code == 0 || strings.Contains(out, "Result: at most") {
				t.Errorf("%s cut at byte %d: exit %d\n%s", rel, n, code, out)
			}
		}
		os.WriteFile(target, full, 0o644)
	}
	if runs < 200 {
		t.Fatalf("only %d cuts", runs)
	}
}
