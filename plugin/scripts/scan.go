package main

import (
	"bytes"
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// inputError means the input can't be trusted. main prints it and exits 3
// without a result.
type inputError struct{ msg string }

func (e *inputError) Error() string { return e.msg }

func refuse(format string, args ...any) error {
	return &inputError{msg: fmt.Sprintf(format, args...)}
}

// sourceFile is one parsed Go file.
type sourceFile struct {
	path string // as shown to the user
	dir  string // absolute directory, used to group packages
	ast  *ast.File
}

// inputs is every file found in the arguments, split by kind.
type inputs struct {
	goFiles   []string
	yamlFiles []string
	// skipped are folders that hold a Helm chart or a kustomization,
	// which can't be read without rendering them first.
	skipped []string
}

// collectFiles expands the arguments into Go and YAML files. Folders are
// walked recursively, skipping vendor, testdata, node_modules and hidden
// folders, and folders that hold a Helm chart (Chart.yaml) or a
// kustomization, because their files aren't the objects that get applied.
// Test files are always skipped: a test opens its own pools.
func collectFiles(args []string) (*inputs, error) {
	in := &inputs{}
	seen := map[string]bool{}
	add := func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		if strings.HasSuffix(p, ".go") {
			in.goFiles = append(in.goFiles, p)
		} else {
			in.yamlFiles = append(in.yamlFiles, p)
		}
	}
	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, refuse("%s: %v", arg, errText(err))
		}
		if !info.IsDir() {
			if !strings.HasSuffix(arg, ".go") && !isYAMLName(arg) {
				return nil, refuse("%s: not a .go, .yaml or .yml file or a folder. toomanyclients reads Go source and Kubernetes manifests, not logs or build output. Pass postgresql.conf with --pg-config", arg)
			}
			if strings.HasSuffix(arg, "_test.go") {
				return nil, refuse("%s: a test file. toomanyclients counts the pools a deployed program opens, so pass the program's source", arg)
			}
			add(arg)
			continue
		}
		before := len(in.goFiles) + len(in.yamlFiles)
		err = filepath.WalkDir(arg, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				if p != arg && (name == "vendor" || name == "testdata" || name == "node_modules" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
					return filepath.SkipDir
				}
				for _, marker := range []string{"Chart.yaml", "kustomization.yaml", "kustomization.yml", "Kustomization"} {
					if _, err := os.Stat(filepath.Join(p, marker)); err == nil {
						in.skipped = append(in.skipped, p+" ("+marker+")")
						return filepath.SkipDir
					}
				}
				return nil
			}
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if strings.HasSuffix(name, "_test.go") {
				return nil
			}
			if strings.HasSuffix(name, ".go") || isYAMLName(name) {
				add(p)
			}
			return nil
		})
		if err != nil {
			return nil, refuse("%s: %v", arg, errText(err))
		}
		if len(in.goFiles)+len(in.yamlFiles) == before {
			msg := fmt.Sprintf("%s: no .go, .yaml or .yml files in this folder", arg)
			if len(in.skipped) > 0 {
				msg += ". Skipped " + strings.Join(in.skipped, ", ") + ": render charts with helm template and kustomizations with kustomize build, and pass the output"
			}
			return nil, refuse("%s", msg)
		}
	}
	sort.Strings(in.goFiles)
	sort.Strings(in.yamlFiles)
	return in, nil
}

func isYAMLName(p string) bool {
	return strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")
}

func errText(err error) string {
	if pe, ok := err.(*fs.PathError); ok {
		return pe.Err.Error()
	}
	return err.Error()
}

// failedOutputPrefixes are how output from a failed go command, compiler,
// helm, kubectl or shell starts. Go files also refuse "# ", which starts a
// failed go build's output and is a comment in YAML and postgresql.conf. A file that starts like this was
// overwritten by a redirect, so it is refused with a clear message.
var failedOutputPrefixes = []string{
	"go: ", "go build", "panic: ", "fatal error: ", "--- FAIL", "FAIL\t", "make: ", "make[", "error: ", "Error: ", "ERROR: ", "bash: ", "sh: ",
	"zsh: ", "psql: ", "Traceback", "WARNING: ", "W1", "E1", "I1", "The connection to the server", "Unable to connect", "error validating",
}

// readChecked reads a file and refuses anything that isn't complete text:
// empty files, binary data, failed command output, and files whose last
// line has no newline, which is how a file cut off partway usually ends.
func readChecked(p, what string, extraPrefixes ...string) ([]byte, error) {
	data, err := readText(p, what, extraPrefixes...)
	if err != nil {
		return nil, err
	}
	if data[len(data)-1] != '\n' {
		return nil, refuse("%s: the last line has no newline, so the file may be cut off. If it is complete, add a newline at the end", p)
	}
	return data, nil
}

// readText is readChecked without the newline check.
func readText(p, what string, extraPrefixes ...string) ([]byte, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, refuse("%s: %v", p, errText(err))
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, refuse("%s: the file is empty", p)
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return nil, refuse("%s: binary data, not %s", p, what)
	}
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	for _, prefix := range append(extraPrefixes, failedOutputPrefixes...) {
		if bytes.HasPrefix(trimmed, []byte(prefix)) {
			line, _, _ := strings.Cut(string(trimmed), "\n")
			return nil, refuse("%s: starts like the output of a failed command, not %s: %q", p, what, truncate(line, 80))
		}
	}
	return data, nil
}

// parseGo reads and parses every Go file, refusing anything that isn't
// complete, valid Go source.
func parseGo(paths []string) (*token.FileSet, []*sourceFile, error) {
	fset := token.NewFileSet()
	var out []*sourceFile
	for _, p := range paths {
		data, err := readChecked(p, "Go source", "# ")
		if err != nil {
			return nil, nil, err
		}
		f, err := goparser.ParseFile(fset, p, data, goparser.SkipObjectResolution)
		if err != nil {
			return nil, nil, refuse("%s", firstLine(err.Error()))
		}
		abs, err := filepath.Abs(filepath.Dir(p))
		if err != nil {
			return nil, nil, refuse("%s: %v", p, err)
		}
		out = append(out, &sourceFile{path: p, dir: abs, ast: f})
	}
	return fset, out, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
