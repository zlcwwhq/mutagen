package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/ipc"
	"github.com/mutagen-io/mutagen/pkg/mutagen"
)

const (
	// registryDirectoryName is the name of the directory, inside the Mutagen
	// data directory, where endpoints register local command submission sockets.
	registryDirectoryName = "remote"
	// recordExtension is the extension of registration records.
	recordExtension = ".json"
	// socketExtension is the extension of registration sockets (or named pipe
	// name records on Windows).
	socketExtension = ".sock"
	// probeTimeout bounds liveness probes of registered sockets.
	probeTimeout = 5 * time.Second
)

// record is the on-disk description of a registered endpoint.
type record struct {
	// Version is the Mutagen version of the registering process.
	Version string
	// Session is the session identifier.
	Session string
	// Name is the session name, if any.
	Name string
	// Root is the normalized synchronization root on this system.
	Root string
	// Alpha indicates whether the registered endpoint is alpha.
	Alpha bool
	// Created is the registration time, used to prefer the newest registration
	// when an endpoint's previous executor has not yet noticed disconnection.
	Created time.Time
}

// registration is a record loaded from the registry.
type registration struct {
	record
	// base is the record path without its extension.
	base string
}

// remove removes a registration's record and socket.
func (r *registration) remove() {
	os.Remove(r.base + recordExtension)
	os.Remove(r.base + socketExtension)
}

// registeredListener removes its registration record when closed.
type registeredListener struct {
	net.Listener
	// recordPath is the path to the registration record.
	recordPath string
}

// Close implements net.Listener.Close.
func (l *registeredListener) Close() error {
	os.Remove(l.recordPath)
	return l.Listener.Close()
}

// isDead determines whether a dialing error indicates that no process is
// listening, as opposed to a transient failure.
func isDead(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

// loadRegistrations reads all parseable registration records.
func loadRegistrations(directory string) ([]*registration, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("unable to read command registry: %w", err)
	}
	var result []*registration
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, recordExtension) || strings.HasPrefix(name, ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			continue
		}
		r := &registration{base: filepath.Join(directory, strings.TrimSuffix(name, recordExtension))}
		if json.Unmarshal(data, &r.record) == nil && r.Root != "" {
			result = append(result, r)
		}
	}
	return result, nil
}

// prune removes registrations whose listeners no longer exist.
func prune(directory string) {
	registrations, _ := loadRegistrations(directory)
	for _, r := range registrations {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		connection, err := ipc.DialContext(ctx, r.base+socketExtension)
		cancel()
		if err == nil {
			connection.Close()
		} else if isDead(err) {
			r.remove()
		}
	}
}

// register creates a listener for local command submissions from an endpoint
// with the specified normalized root and records it in the registry. The
// record is removed when the listener is closed.
func register(endpoint *Endpoint, root string) (net.Listener, error) {
	directory, err := filesystem.Mutagen(true, registryDirectoryName)
	if err != nil {
		return nil, fmt.Errorf("unable to compute command registry directory: %w", err)
	}
	prune(directory)

	// Use short random names because socket paths have tight length limits.
	var identifier [8]byte
	if _, err := rand.Read(identifier[:]); err != nil {
		return nil, fmt.Errorf("unable to generate registration identifier: %w", err)
	}
	base := filepath.Join(directory, hex.EncodeToString(identifier[:]))
	listener, err := ipc.NewListener(base + socketExtension)
	if err != nil {
		return nil, fmt.Errorf("unable to create command socket: %w", err)
	}

	// Write the record only once the listener exists, so a record whose socket
	// refuses connections is known to be stale. Rename it into place so that
	// readers never observe a partial record.
	data, err := json.Marshal(&record{
		Version: mutagen.Version,
		Session: endpoint.Session,
		Name:    endpoint.Name,
		Root:    root,
		Alpha:   endpoint.Alpha,
		Created: time.Now(),
	})
	if err != nil {
		listener.Close()
		return nil, fmt.Errorf("unable to encode registration: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".record-*")
	if err != nil {
		listener.Close()
		return nil, fmt.Errorf("unable to create registration: %w", err)
	}
	_, err = temporary.Write(data)
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary.Name(), base+recordExtension)
	}
	if err != nil {
		os.Remove(temporary.Name())
		listener.Close()
		return nil, fmt.Errorf("unable to write registration: %w", err)
	}
	return &registeredListener{listener, base + recordExtension}, nil
}

// Target describes the endpoint from which a command is submitted.
type Target struct {
	// Session is the session identifier.
	Session string
	// Name is the session name, if any.
	Name string
	// Alpha indicates whether the submitting endpoint is alpha, in which case
	// the command runs on beta (and vice versa).
	Alpha bool
	// Root is the submitting endpoint's synchronization root.
	Root string
	// Path is the slash-separated submission directory relative to Root. It is
	// empty if the submission directory is the root or lies outside it.
	Path string
	// Contained indicates whether the submission directory lies inside Root,
	// in which case Path identifies the corresponding directory on the
	// opposite endpoint.
	Contained bool
}

// candidate is a registration matching a submission.
type candidate struct {
	*registration
	// contained indicates whether the submission directory is inside the root.
	contained bool
	// depth is the length of the resolved root if it contains the submission
	// directory (and zero otherwise), used to rank nested roots. Roots that
	// don't contain the directory rank equally so that ambiguity among them is
	// reported rather than resolved by root length.
	depth int
	// path is the slash-separated submission directory relative to the root.
	path string
}

// relativePath determines whether a directory is inside a root and, if so,
// computes its slash-separated relative path.
func relativePath(root, directory string) (string, bool) {
	relative, err := filepath.Rel(root, directory)
	if err != nil || filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	} else if relative == "." {
		return "", true
	}
	return filepath.ToSlash(relative), true
}

// resolveSymbolicLinks resolves a path when possible, matching the physical
// paths returned by os.Getwd.
func resolveSymbolicLinks(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// describe formats a session for diagnostic messages.
func describe(session, name string) string {
	if name != "" {
		return fmt.Sprintf("%s (%s)", name, session)
	}
	return session
}

// Dial connects to the command route of the registered endpoint whose root
// most specifically contains the specified directory. If session is non-empty,
// only endpoints of sessions with that identifier or name are considered, and
// the directory may lie outside the root. The returned connection has
// completed its version handshake and is suitable for Execute.
func Dial(ctx context.Context, directory, session string) (net.Conn, *Target, error) {
	registry, err := filesystem.Mutagen(false, registryDirectoryName)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to compute command registry directory: %w", err)
	}
	registrations, err := loadRegistrations(registry)
	if err != nil {
		return nil, nil, err
	}

	// Identify matching registrations.
	directory = resolveSymbolicLinks(directory)
	var candidates []*candidate
	var mismatched []string
	for _, r := range registrations {
		if session != "" && r.Session != session && r.Name != session {
			continue
		}
		root := resolveSymbolicLinks(r.Root)
		path, contained := relativePath(root, directory)
		if !contained && session == "" {
			continue
		} else if r.Version != mutagen.Version {
			mismatched = append(mismatched, r.Version)
			continue
		}
		var depth int
		if contained {
			depth = len(root)
		}
		candidates = append(candidates, &candidate{r, contained, depth, path})
	}

	// Rank containing roots first, then more specific containing roots, then
	// newer registrations so that a lingering executor from a previous
	// connection is superseded by its replacement.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].contained != candidates[j].contained {
			return candidates[i].contained
		} else if candidates[i].depth != candidates[j].depth {
			return candidates[i].depth > candidates[j].depth
		}
		return candidates[i].Created.After(candidates[j].Created)
	})

	// Process candidates in tiers of equal rank. Dead registrations are pruned
	// and the next registration for the same endpoint is considered instead.
	for start := 0; start < len(candidates); {
		end := start + 1
		for end < len(candidates) && candidates[end].contained == candidates[start].contained &&
			candidates[end].depth == candidates[start].depth {
			end++
		}
		type liveEndpoint struct {
			connection net.Conn
			candidate  *candidate
		}
		var live []liveEndpoint
		resolved := make(map[string]bool)
		closeLive := func() {
			for _, l := range live {
				l.connection.Close()
			}
		}
		for _, c := range candidates[start:end] {
			key := fmt.Sprintf("%s/%t", c.Session, c.Alpha)
			if resolved[key] {
				continue
			}
			connection, err := ipc.DialContext(ctx, c.base+socketExtension)
			if err != nil {
				if isDead(err) {
					c.remove()
					continue
				}
				closeLive()
				return nil, nil, fmt.Errorf("unable to connect to session %s: %w", describe(c.Session, c.Name), err)
			}
			resolved[key] = true
			live = append(live, liveEndpoint{connection, c})
		}
		if len(live) > 1 {
			closeLive()
			var sessions []string
			described := make(map[string]bool)
			for _, l := range live {
				if !described[l.candidate.Session] {
					described[l.candidate.Session] = true
					sessions = append(sessions, describe(l.candidate.Session, l.candidate.Name))
				}
			}
			if session != "" && len(sessions) == 1 {
				return nil, nil, fmt.Errorf("both endpoints of session %s are on this system; run from inside one of their roots", sessions[0])
			} else if session != "" {
				return nil, nil, fmt.Errorf("multiple sessions match %q (%s); use a session identifier to select one", session, strings.Join(sessions, ", "))
			}
			return nil, nil, fmt.Errorf("multiple sessions contain %s (%s); use --session to select one", directory, strings.Join(sessions, ", "))
		} else if len(live) == 1 {
			connection, c := live[0].connection, live[0].candidate
			stop := context.AfterFunc(ctx, func() { connection.Close() })
			err := mutagen.ClientVersionHandshake(connection)
			if !stop() {
				return nil, nil, ctx.Err()
			} else if err != nil {
				connection.Close()
				return nil, nil, fmt.Errorf("unable to handshake with session %s: %w", describe(c.Session, c.Name), err)
			}
			return connection, &Target{
				Session:   c.Session,
				Name:      c.Name,
				Alpha:     c.Alpha,
				Root:      c.Root,
				Path:      c.path,
				Contained: c.contained,
			}, nil
		}
		start = end
	}

	// Explain why no route was found.
	if len(mismatched) > 0 {
		return nil, nil, fmt.Errorf("matching sessions are registered by Mutagen %s, but this is Mutagen %s; use the remote executable installed with that version",
			strings.Join(mismatched, ", "), mutagen.Version)
	} else if session != "" {
		return nil, nil, fmt.Errorf("no connected session matching %q has remote commands enabled on this system", session)
	}
	return nil, nil, fmt.Errorf("no connected session with remote commands enabled contains %s", directory)
}
