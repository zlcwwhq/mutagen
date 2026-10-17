package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// environmentValue reads the last assignment, matching exec.Cmd semantics.
func environmentValue(environment []string, name string) string {
	for i := len(environment) - 1; i >= 0; i-- {
		key, value, ok := strings.Cut(environment[i], "=")
		if ok && (key == name || runtime.GOOS == "windows" && strings.EqualFold(key, name)) {
			return value
		}
	}
	return ""
}

// lookPath resolves executables using the command's working directory and PATH,
// rather than the agent process's working directory and PATH.
func lookPath(name, directory string, environment []string) (string, error) {
	paths := []string{name}
	if filepath.Base(name) == name {
		paths = nil
		for _, entry := range filepath.SplitList(environmentValue(environment, "PATH")) {
			paths = append(paths, filepath.Join(entry, name))
		}
	}
	extensions := []string{""}
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		value := environmentValue(environment, "PATHEXT")
		if value == "" {
			value = ".COM;.EXE;.BAT;.CMD"
		}
		extensions = strings.Split(value, ";")
	}
	for _, candidate := range paths {
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(directory, candidate)
		}
		for _, extension := range extensions {
			path := candidate + extension
			if info, err := os.Stat(path); err == nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0111 != 0) {
				return path, nil
			}
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}
