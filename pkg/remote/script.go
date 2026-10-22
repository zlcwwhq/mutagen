package remote

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
)

const (
	// scriptsDirectoryName is the name of the directory, inside the Mutagen
	// data directory, that holds the files of scripts run in the foreground
	// while they run.
	scriptsDirectoryName = "remote-scripts"
	// DefaultScriptInterpreter is the interpreter of scripts without a #! line.
	DefaultScriptInterpreter = "bash"
	// scriptRetention is the age after which script files left behind, such
	// as by an executing process that was killed, are removed.
	scriptRetention = 24 * time.Hour
)

// parseShebang parses a script's #! line, if any, as Linux does: the line names
// an interpreter, optionally followed by a single argument, which consists of
// the rest of the line, with surrounding blanks removed. Unlike Linux, a
// trailing carriage return is removed as well, so that scripts with Windows
// line endings name their interpreter correctly.
func parseShebang(contents []byte) (interpreter, argument string, found bool, err error) {
	if !bytes.HasPrefix(contents, []byte("#!")) {
		return "", "", false, nil
	}
	line := contents[2:]
	if end := bytes.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	text := strings.Trim(strings.TrimSuffix(string(line), "\r"), " \t")
	if text == "" {
		return "", "", true, errors.New("the script's #! line doesn't name an interpreter")
	} else if !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		return "", "", true, errors.New("the script's #! line contains invalid UTF-8 or a null byte")
	}
	if blank := strings.IndexAny(text, " \t"); blank >= 0 {
		return text[:blank], strings.Trim(text[blank+1:], " \t"), true, nil
	}
	return text, "", true, nil
}

// pruneScripts removes script files older than scriptRetention from the
// specified directory. Removing the file of a script that's still running is
// harmless, since its interpreter has already opened it.
func pruneScripts(directory string) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > scriptRetention {
			os.Remove(filepath.Join(directory, entry.Name()))
		}
	}
}

// writeScript writes a script to a new file at the specified path, or to a new
// temporary file if the path is empty, and returns the file's path along with
// the arguments that run the script, with the specified arguments, using the
// interpreter named by its #! line or DefaultScriptInterpreter. The file isn't
// executable: the interpreter reads it, so it may reside on a filesystem that
// doesn't permit execution.
func writeScript(script *Script, arguments []string, path string) (string, []string, error) {
	if runtime.GOOS == "windows" {
		return "", nil, errors.New("scripts aren't supported on Windows endpoints")
	}
	interpreter, argument, found, err := parseShebang(script.Contents)
	if err != nil {
		return "", nil, err
	} else if !found {
		interpreter = DefaultScriptInterpreter
	}
	var file *os.File
	if path == "" {
		var directory string
		if directory, err = filesystem.Mutagen(true, scriptsDirectoryName); err != nil {
			return "", nil, fmt.Errorf("unable to compute script directory: %w", err)
		}
		pruneScripts(directory)
		file, err = os.CreateTemp(directory, "script-*")
	} else {
		file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	}
	if err != nil {
		return "", nil, fmt.Errorf("unable to create script file: %w", err)
	}
	_, err = file.Write(script.Contents)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(file.Name())
		return "", nil, fmt.Errorf("unable to write script file: %w", err)
	}
	result := []string{interpreter}
	if argument != "" {
		result = append(result, argument)
	}
	result = append(result, file.Name())
	return file.Name(), append(result, arguments...), nil
}
