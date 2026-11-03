package ignore

import "strings"

// Excluded evaluates a root-relative path as traversal would, including its
// parents, whether or not they exist. It returns the shallowest path that keeps
// traversal from reaching the path, which is empty if there's none, or an error
// if the rules that apply to the path can't be read.
func Excluded(ignorer Ignorer, path string, directory bool) (string, error) {
	loader, loadsRules := ignorer.(DirectoryIgnorer)
	if path == "" {
		if loadsRules && directory {
			return "", loader.EnsureDirectory("")
		}
		return "", nil
	}
	components := strings.Split(path, "/")
	masked := false
	parent := ""
	for c := range components {
		if loadsRules {
			if err := loader.EnsureDirectory(parent); err != nil {
				return "", err
			}
		}
		current := strings.Join(components[:c+1], "/")
		status, traverse := ignorer.Ignore(current, directory || c < len(components)-1)
		switch status {
		case IgnoreStatusIgnored:
			masked = true
		case IgnoreStatusUnignored:
			masked = false
		}
		if masked && !traverse {
			return current, nil
		}
		parent = current
	}
	return "", nil
}
