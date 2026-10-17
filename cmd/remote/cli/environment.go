package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// loadEnvironment loads literal NAME=VALUE entries from files and command line
// options. File values precede command line values, allowing explicit overrides.
func loadEnvironment(paths, entries []string) ([]string, error) {
	var result []string
	for _, path := range paths {
		fileEntries, err := readEnvironmentFile(path)
		if err != nil {
			return nil, err
		}
		result = append(result, fileEntries...)
	}
	for _, entry := range entries {
		// A bare name explicitly forwards a variable from the local process.
		if !strings.Contains(entry, "=") {
			value, ok := os.LookupEnv(entry)
			if !ok {
				return nil, fmt.Errorf("local environment variable %q is not set", entry)
			}
			entry += "=" + value
		}
		result = append(result, entry)
	}
	return result, nil
}

// readEnvironmentFile reads NAME=VALUE lines, skipping blank lines and comments.
func readEnvironmentFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("unable to open environment file: %w", err)
	}
	defer file.Close()
	var result []string
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		entry := strings.TrimSuffix(scanner.Text(), "\r")
		if trimmed := strings.TrimSpace(entry); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.TrimSpace(key) != key {
			return nil, fmt.Errorf("invalid environment entry in %s at line %d (expected NAME=VALUE)", path, line)
		}
		result = append(result, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("unable to read environment file: %w", err)
	}
	return result, nil
}
