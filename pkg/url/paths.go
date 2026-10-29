package url

// IsWindowsDrivePath identifies an absolute path with a Windows drive letter,
// independently of the current platform, which is how URLs and other raw
// strings recognize Windows paths. Drive-relative paths such as C:work are
// deliberately excluded. UNC paths aren't recognized here, since // also starts
// a POSIX path, so their handling remains specific to each caller (see
// IsWindowsUNCPath).
func IsWindowsDrivePath(path string) bool {
	// These will all be single-byte runes, so we can do direct byte access and
	// comparison.
	return len(path) >= 3 &&
		((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')) &&
		path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}

// IsWindowsUNCPath identifies a UNC prefix in either separator form. Callers
// interpreting paths on unknown remote systems must decide whether a leading
// double forward slash denotes a UNC path or a POSIX path.
func IsWindowsUNCPath(path string) bool {
	return len(path) >= 2 && (path[0] == '\\' && path[1] == '\\' || path[0] == '/' && path[1] == '/')
}

// WindowsVolumeLength returns the length of an absolute Windows path's drive
// or UNC host/share prefix, independently of the current platform. It returns
// zero for other paths. Separating the prefix before cleaning prevents .. from
// escaping above a drive or share. An incomplete UNC prefix is retained in
// full; syntax recognition does not validate whether its server/share exists.
func WindowsVolumeLength(path string) int {
	if IsWindowsDrivePath(path) {
		return 2
	} else if !IsWindowsUNCPath(path) {
		return 0
	}
	separators := 0
	for i := 2; i < len(path); i++ {
		if path[i] == '/' || path[i] == '\\' {
			separators++
			if separators == 2 {
				return i
			}
		}
	}
	return len(path)
}
