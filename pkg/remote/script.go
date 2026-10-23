package remote

import (
	"bytes"
	"errors"
	"fmt"
	"os"
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
	// scriptFilePrefix is the prefix of the names of the files of scripts run
	// in the foreground, which is followed by a random string.
	scriptFilePrefix = "script-"
	// DefaultScriptInterpreter is the interpreter of scripts without a #! line.
	DefaultScriptInterpreter = "bash"
	// scriptRetention is the age after which the files of scripts run in the
	// foreground that are no longer in use, such as those left behind by an
	// executing process that was killed, are removed (see pruneScripts).
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

// writeScript writes a script to a new file at the specified path, or to a new
// temporary file if the path is empty, and returns the file's path along with
// the arguments that run the script, with the specified arguments, using the
// interpreter named by its #! line or DefaultScriptInterpreter. The file isn't
// executable: the interpreter reads it, so it may reside on a filesystem that
// doesn't permit execution. For a temporary file, it also returns the open file
// that holds the file's lock, if it could be locked, which keeps the file from
// being removed while the script runs (see lockScript). The caller must close
// it once the script has exited, after removing the file.
func writeScript(script *Script, arguments []string, path string) (string, *os.File, []string, error) {
	if runtime.GOOS == "windows" {
		return "", nil, nil, errors.New("scripts aren't supported on Windows endpoints")
	}
	interpreter, argument, found, err := parseShebang(script.Contents)
	if err != nil {
		return "", nil, nil, err
	} else if !found {
		interpreter = DefaultScriptInterpreter
	}

	// Create the file, locking a temporary file before writing it. Only files
	// older than scriptRetention are pruned, so the new file can't be removed
	// before it's locked.
	var file, lock *os.File
	if path == "" {
		var directory string
		if directory, err = filesystem.Mutagen(true, scriptsDirectoryName); err != nil {
			return "", nil, nil, fmt.Errorf("unable to compute script directory: %w", err)
		}
		pruneScripts(directory)
		if file, err = os.CreateTemp(directory, scriptFilePrefix+"*"); err == nil {
			lock = lockScript(file.Name())
		}
	} else {
		file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	}
	if err != nil {
		return "", nil, nil, fmt.Errorf("unable to create script file: %w", err)
	}

	// Write the script.
	_, err = file.Write(script.Contents)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(file.Name())
		if lock != nil {
			lock.Close()
		}
		return "", nil, nil, fmt.Errorf("unable to write script file: %w", err)
	}
	result := []string{interpreter}
	if argument != "" {
		result = append(result, argument)
	}
	result = append(result, file.Name())
	return file.Name(), lock, append(result, arguments...), nil
}
