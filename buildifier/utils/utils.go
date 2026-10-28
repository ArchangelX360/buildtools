/*
Copyright 2020 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package utils contains shared methods that can be used by different implementations of
// buildifier binary
package utils

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/bazel-contrib/buildtools/v10/build"
	"github.com/bazel-contrib/buildtools/v10/warn"
)

func isStarlarkFile(name string) bool {
	ext := filepath.Ext(name)
	switch ext {
	case ".bazel", ".BUILD", ".bzl", ".sky", ".star":
		return true
	}

	switch ext {
	case ".oss":
		return strings.HasPrefix(name, "BUILD.") || strings.HasPrefix(name, "WORKSPACE.")
	}

	return name == "BUILD" || name == "WORKSPACE" || name == "WORKSPACE.bzlmod"
}

func skip(info os.FileInfo) bool {
	return info.IsDir() && info.Name() == ".git"
}

// ExpandDirectories takes a list of file/directory names and returns a list with file names
// by traversing each directory recursively and searching for relevant Starlark files. Paths
// that the optional excluder excludes are skipped, without reading excluded directories.
//
// Directories are read in parallel; the files are returned in the order of a sequential
// depth-first walk visiting directory entries in lexical order, like filepath.Walk.
func ExpandDirectories(args *[]string, excluder *Excluder) ([]string, error) {
	files := []string{}
	for _, arg := range *args {
		info, err := os.Stat(arg)
		if err != nil {
			return []string{}, err
		}
		if !info.IsDir() {
			files = append(files, arg)
			continue
		}
		found, err := walkStarlarkFiles(arg, excluder)
		if err != nil {
			return []string{}, err
		}
		files = append(files, found...)
	}
	return files, nil
}

// walkConcurrency bounds the number of directories read at the same time.
var walkConcurrency = 4 * runtime.GOMAXPROCS(0)

type starlarkWalker struct {
	excluder *Excluder
	sem      chan struct{}
	wg       sync.WaitGroup

	mu    sync.Mutex
	files []string
	err   error
}

func walkStarlarkFiles(root string, excluder *Excluder) ([]string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	w := &starlarkWalker{excluder: excluder, sem: make(chan struct{}, walkConcurrency)}
	w.wg.Add(1)
	go w.walkDir(filepath.Clean(root), absRoot)
	w.wg.Wait()
	if w.err != nil {
		return nil, w.err
	}
	sortLikeWalk(w.files)
	return w.files, nil
}

func (w *starlarkWalker) walkDir(dir, absDir string) {
	defer w.wg.Done()
	w.sem <- struct{}{}
	entries, err := os.ReadDir(dir)
	<-w.sem
	if err != nil {
		w.fail(err)
		return
	}

	var files []string
	for _, entry := range entries {
		name := entry.Name()
		filename := filepath.Join(dir, name)
		absFilename := filepath.Join(absDir, name)
		// Check exclusions before reading anything, so that excluded paths never
		// fail the run, e.g. when they are unreadable.
		if w.excluder.excludedAbs(absFilename) {
			continue
		}
		if entry.IsDir() {
			if name == ".git" {
				continue
			}
			w.wg.Add(1)
			go w.walkDir(filename, absFilename)
			continue
		}
		if !isStarlarkFile(name) {
			continue
		}
		// Don't traverse into directory symlinks such as bazel-foo.bzl
		// for a project called foo.bzl.
		if entry.Type()&os.ModeSymlink != 0 {
			stat, err := os.Stat(filename)
			if err != nil || stat.IsDir() {
				continue
			}
		}
		files = append(files, filename)
	}

	w.mu.Lock()
	w.files = append(w.files, files...)
	w.mu.Unlock()
}

func (w *starlarkWalker) fail(err error) {
	w.mu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.mu.Unlock()
}

// sortLikeWalk sorts paths in the order filepath.Walk visits them: segment by
// segment, so that a directory's entries are sorted by name.
func sortLikeWalk(paths []string) {
	keys := make(map[string]string, len(paths))
	for _, p := range paths {
		keys[p] = strings.ReplaceAll(p, string(filepath.Separator), "\x00")
	}
	sort.Slice(paths, func(i, j int) bool { return keys[paths[i]] < keys[paths[j]] })
}

// GetParser returns a parser for a given file type
func GetParser(inputType string) func(filename string, data []byte) (*build.File, error) {
	switch inputType {
	case "build":
		return build.ParseBuild
	case "bzl":
		return build.ParseBzl
	case "auto":
		return build.Parse
	case "workspace":
		return build.ParseWorkspace
	case "module":
		return build.ParseModule
	case "repo":
		return build.ParseRepo
	case "vendor":
		return build.ParseVendor
	default:
		return build.ParseDefault
	}
}

// getFileReader returns a *FileReader object that reads files from the local
// filesystem if the workspace root is known.
func getFileReader(workspaceRoot string) *warn.FileReader {
	if workspaceRoot == "" {
		return nil
	}

	readFile := func(filename string) ([]byte, error) {
		// Use OS-specific path separators
		filename = strings.ReplaceAll(filename, "/", string(os.PathSeparator))
		path := filepath.Join(workspaceRoot, filename)

		return os.ReadFile(path)
	}

	return warn.NewFileReader(readFile)
}

// Lint calls the linter and returns a list of unresolved findings
func Lint(f *build.File, lint string, warningsList *[]string, verbose bool) []*warn.Finding {
	fileReader := getFileReader(f.WorkspaceRoot)

	switch lint {
	case "warn":
		return warn.FileWarnings(f, *warningsList, nil, warn.ModeWarn, fileReader)
	case "fix":
		warn.FixWarnings(f, *warningsList, verbose, fileReader)
	}
	return nil
}
