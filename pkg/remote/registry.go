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
	"runtime"
	"slices"
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
	// refusalConfirmationTimeout is how long refused connections to a
	// registered socket are retried, where refusals are ambiguous (see
	// ambiguousRefusals), before the refusal is reported.
	refusalConfirmationTimeout = 250 * time.Millisecond
	// refusalRetryInterval is the interval between those retries.
	refusalRetryInterval = 10 * time.Millisecond
)

// ambiguousRefusals indicates whether connections to a Unix domain socket
// whose listener's backlog is full are refused, as on systems derived from BSD,
// including macOS, which then can't be told apart from connections to a socket
// that no process listens on. Linux fails with EAGAIN instead. It's a variable
// so that tests can enable it on other systems.
var ambiguousRefusals = slices.Contains([]string{"darwin", "ios", "freebsd", "netbsd", "openbsd", "dragonfly"}, runtime.GOOS)

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
	// SessionCreated is the session's creation time (see
	// Endpoint.SessionCreated).
	SessionCreated time.Time
	// Ignores are the session's ignores, if known (see Endpoint.Ignores).
	Ignores *Ignores `json:",omitempty"`
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
// listening, as opposed to a transient failure. Refusals are only conclusive
// once they persist (see dialRegistration).
func isDead(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

// dialRegistration connects to a registered socket. Where refusals are
// ambiguous (see ambiguousRefusals), refused connections are retried until
// refusalConfirmationTimeout elapses, since a full backlog, such as while many
// submissions connect at once, drains as the listener accepts connections,
// and a live registration would otherwise be removed as dead (see isDead).
// Cancellation during the retries is reported instead of the refusal.
func dialRegistration(ctx context.Context, path string) (net.Conn, error) {
	deadline := time.Now().Add(refusalConfirmationTimeout)
	for {
		connection, err := ipc.DialContext(ctx, path)
		if err == nil || !ambiguousRefusals || !errors.Is(err, syscall.ECONNREFUSED) || !time.Now().Before(deadline) {
			return connection, err
		}
		select {
		case <-time.After(refusalRetryInterval):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
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
		connection, err := dialRegistration(ctx, r.base+socketExtension)
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
		Version:        mutagen.Version,
		Session:        endpoint.Session,
		Name:           endpoint.Name,
		Root:           root,
		Alpha:          endpoint.Alpha,
		Created:        time.Now(),
		SessionCreated: endpoint.SessionCreated,
		Ignores:        endpoint.Ignores,
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
	// Overlapping labels the sessions whose roots contain the submission
	// directory and that synchronize it (see Session.Label), starting with the
	// located session and continuing in the order of preference, if there are
	// several and the session was located without being selected (by
	// Selection.Session or a matching Selection.Default), since the submission
	// may then have been meant for another one. It's empty otherwise, and for
	// pinned selections.
	Overlapping []string
	// Ignores are the session's ignores, if known (see Endpoint.Ignores), which
	// determine the paths that the session doesn't synchronize.
	Ignores *Ignores
}

// Session describes a session endpoint on this system through which
// operations can be submitted, as listed by Selection.Sessions.
type Session struct {
	// Session is the session identifier.
	Session string
	// Name is the session name, if any.
	Name string
	// Label is the session name, or the identifier if the session is unnamed
	// or another registered session shares its name, so that it selects the
	// session unambiguously.
	Label string
	// Alpha indicates whether the endpoint is alpha.
	Alpha bool
	// Root is the endpoint's synchronization root.
	Root string
	// Version is the Mutagen version that registered the endpoint, which must
	// match this version for submissions through it.
	Version string
	// Contained indicates whether Root contains the directory.
	Contained bool
	// Excluded indicates whether the session's ignores exclude the directory,
	// or a parent of it inside Root, so that the session doesn't synchronize
	// it (see Selection.Corresponding).
	Excluded bool
	// Selected indicates whether the selection locates the endpoint for the
	// directory.
	Selected bool
}

// SessionVariable is the environment variable that selects a session, by
// identifier or name, for the remote executable when none is selected
// explicitly. Unlike an explicit selection, it never redirects submissions from
// inside one session's root to another session (see Selection.Default).
const SessionVariable = "MUTAGEN_REMOTE_SESSION"

// Selection determines the session, and the endpoint of that session, through
// which Dial submits an operation from a directory.
type Selection struct {
	// Session is the identifier or name of an explicitly selected session. If
	// non-empty, then only that session's endpoints are considered, and the
	// directory may lie outside their roots, in which case the submission
	// applies to the root, although endpoints whose roots contain the
	// directory are preferred.
	Session string
	// Default is the value of SessionVariable, if any. It's ignored if Session
	// is non-empty. Otherwise, it selects among the sessions whose roots
	// contain the directory: if any of them matches it, then the most specific
	// of their roots is used, and if none does, then it's ignored, and the
	// most specific containing root is used, as if it were unset. Only if no
	// session's root contains the directory does it select a session in the
	// same way as Session.
	Default string
	// Corresponding indicates that the operation applies to the directory
	// corresponding to the submission directory, as commands do unless their
	// working directory is specified explicitly. Dial then refuses to submit
	// it through a session selected by Session whose roots don't contain the
	// directory if another session's root does, since it would apply to the
	// root of the selected session, rather than to anything corresponding to
	// the directory. Dial also refuses to submit it through a session whose
	// ignores exclude the directory or a parent of it inside the root (see
	// Ignores), since the session doesn't synchronize the corresponding
	// directory. Such a directory is typically the root of a nested session
	// that isn't connected for remote commands on this system, and through
	// which the submission would otherwise have been made.
	Corresponding bool
	// Target, if non-nil, restricts the selection to the session and endpoint
	// that it identifies, which were typically located by a previous
	// submission, so that related submissions can't reach different sessions
	// as sessions connect and disconnect in between. Dial then fails if that
	// endpoint is no longer registered, rather than selecting another, and
	// Session, Default, and Corresponding are ignored.
	Target *Target
}

// candidate is a registration related to a submission directory.
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

// filter returns the candidates that satisfy all of the specified predicates.
func filter(candidates []*candidate, predicates ...func(*candidate) bool) []*candidate {
	var result []*candidate
	for _, c := range candidates {
		satisfied := true
		for _, predicate := range predicates {
			satisfied = satisfied && predicate(c)
		}
		if satisfied {
			result = append(result, c)
		}
	}
	return result
}

// containing is a candidate predicate that identifies roots containing the
// submission directory.
func containing(c *candidate) bool {
	return c.contained
}

// excluding is a candidate predicate that identifies roots that don't contain
// the submission directory.
func excluding(c *candidate) bool {
	return !c.contained
}

// matching returns a candidate predicate that identifies the registrations of
// sessions with the specified identifier or name.
func matching(session string) func(*candidate) bool {
	return func(c *candidate) bool {
		return c.Session == session || c.Name == session
	}
}

// versions returns the distinct Mutagen versions of candidates.
func versions(candidates []*candidate) []string {
	var result []string
	for _, c := range candidates {
		if !slices.Contains(result, c.Version) {
			result = append(result, c.Version)
		}
	}
	return result
}

// liveEndpoint is a connection to the submission socket of a registered
// endpoint, which hasn't yet performed a version handshake.
type liveEndpoint struct {
	// connection is the connection to the endpoint.
	connection net.Conn
	// candidate is the endpoint's registration.
	candidate *candidate
}

// closeEndpoints closes connections to live endpoints.
func closeEndpoints(endpoints []liveEndpoint) {
	for _, e := range endpoints {
		e.connection.Close()
	}
}

// describeEndpoints describes the distinct sessions of live endpoints.
func describeEndpoints(endpoints []liveEndpoint) []string {
	var result []string
	described := make(map[string]bool)
	for _, e := range endpoints {
		if !described[e.candidate.Session] {
			described[e.candidate.Session] = true
			result = append(result, describe(e.candidate.Session, e.candidate.Name))
		}
	}
	return result
}

// probeTier connects to the newest live registration of each endpoint among
// equally ranked candidates, so that a lingering executor from a previous
// connection is superseded by its replacement. Dead registrations are pruned,
// and the next registration for the same endpoint is considered instead.
func probeTier(ctx context.Context, tier []*candidate) ([]liveEndpoint, error) {
	var live []liveEndpoint
	resolved := make(map[string]bool)
	for _, c := range tier {
		key := fmt.Sprintf("%s/%t", c.Session, c.Alpha)
		if resolved[key] {
			continue
		}
		connection, err := dialRegistration(ctx, c.base+socketExtension)
		if err != nil {
			if isDead(err) {
				c.remove()
				continue
			}
			closeEndpoints(live)
			return nil, fmt.Errorf("unable to connect to session %s: %w", describe(c.Session, c.Name), err)
		}
		resolved[key] = true
		live = append(live, liveEndpoint{connection, c})
	}
	return live, nil
}

// locate ranks candidates and connects to the live endpoints in the first tier
// of equally ranked candidates that has any, returning none if no candidate is
// live. Containing roots rank first, then more specific containing roots, and
// registrations of the same endpoint are ranked by age (see probeTier). The
// caller resolves or reports ambiguity among several live endpoints.
func locate(ctx context.Context, candidates []*candidate) ([]liveEndpoint, error) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].contained != candidates[j].contained {
			return candidates[i].contained
		} else if candidates[i].depth != candidates[j].depth {
			return candidates[i].depth > candidates[j].depth
		}
		return candidates[i].Created.After(candidates[j].Created)
	})
	for start := 0; start < len(candidates); {
		end := start + 1
		for end < len(candidates) && candidates[end].contained == candidates[start].contained &&
			candidates[end].depth == candidates[start].depth {
			end++
		}
		if live, err := probeTier(ctx, candidates[start:end]); err != nil || len(live) > 0 {
			return live, err
		}
		start = end
	}
	return nil, nil
}

// precedes orders candidates whose roots contain the submission directory
// equally specifically: the session created first is preferred, with ties
// broken by identifier, so that the preference doesn't change as sessions
// reconnect or more sessions are created.
func precedes(a, b *candidate) bool {
	if !a.SessionCreated.Equal(b.SessionCreated) {
		return a.SessionCreated.Before(b.SessionCreated)
	}
	return a.Session < b.Session
}

// synchronizingSessions returns one live registration for each session among
// candidates whose roots contain the submission directory, most specific
// first, and then in the order of preference (see precedes). Dead
// registrations are pruned, while registrations whose liveness can't be
// determined are assumed to be live. Registrations are only probed for
// liveness, without a version handshake, so registrations by other versions of
// Mutagen are included.
func synchronizingSessions(ctx context.Context, candidates []*candidate) ([]*candidate, error) {
	candidates = filter(candidates, containing)
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].depth != candidates[j].depth {
			return candidates[i].depth > candidates[j].depth
		}
		return precedes(candidates[i], candidates[j])
	})
	return liveSessions(ctx, candidates)
}

// liveSessions returns one live registration for each session among
// candidates, in their order. Dead registrations are pruned, while
// registrations whose liveness can't be determined are assumed to be live.
func liveSessions(ctx context.Context, candidates []*candidate) ([]*candidate, error) {
	var result []*candidate
	found := make(map[string]bool)
	for _, c := range candidates {
		if found[c.Session] {
			continue
		}
		connection, err := dialRegistration(ctx, c.base+socketExtension)
		if err == nil {
			connection.Close()
		} else if ctx.Err() != nil {
			return nil, ctx.Err()
		} else if isDead(err) {
			c.remove()
			continue
		}
		found[c.Session] = true
		result = append(result, c)
	}
	return result, nil
}

// synchronizing describes the sessions that synchronizingSessions returns.
func synchronizing(ctx context.Context, candidates []*candidate) ([]string, error) {
	sessions, err := synchronizingSessions(ctx, candidates)
	if err != nil {
		return nil, err
	}
	result := make([]string, len(sessions))
	for s, c := range sessions {
		result[s] = describe(c.Session, c.Name)
	}
	return result, nil
}

// labeler returns a function that labels sessions by name, or by identifier if
// they're unnamed or another session among the candidates shares their name,
// so that labels select sessions unambiguously (see matching).
func labeler(candidates []*candidate) func(*candidate) string {
	named := make(map[string]map[string]bool)
	for _, c := range candidates {
		if c.Name != "" {
			if named[c.Name] == nil {
				named[c.Name] = make(map[string]bool)
			}
			named[c.Name][c.Session] = true
		}
	}
	return func(c *candidate) string {
		if c.Name != "" && len(named[c.Name]) == 1 {
			return c.Name
		}
		return c.Session
	}
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

// excludedError reports that an operation corresponding to a directory can't
// be submitted through the session of a candidate whose ignores exclude it,
// given the excluded slash-separated path relative to the session's root (see
// Ignores.Excluded).
func excludedError(directory string, c *candidate, excluded string) error {
	session := describe(c.Session, c.Name)
	excludedDirectory := filepath.Join(resolveSymbolicLinks(c.Root), filepath.FromSlash(excluded))
	location := fmt.Sprintf("%s is ignored by session %s", directory, session)
	if excluded != c.path {
		location = fmt.Sprintf("%s is inside %s, which session %s ignores", directory, excludedDirectory, session)
	}
	return fmt.Errorf("%s, so that session doesn't synchronize the corresponding directory on its opposite endpoint; if %s is the root of another session, then that session is paused, disconnected, or doesn't run commands submitted from this side, so commands can't run through it until it does; otherwise, to run the command in the corresponding directory anyway, use --cwd .",
		location, excludedDirectory)
}

// Dial submits the specified operation through the endpoint that a selection of
// the specified session, if non-empty, locates for the specified directory,
// with the session named by SessionVariable, if set, as the selection's
// default (see Selection.Dial).
func Dial(ctx context.Context, directory, session string, operation Operation) (net.Conn, *Target, error) {
	selection := &Selection{Session: session, Default: os.Getenv(SessionVariable)}
	return selection.Dial(ctx, directory, operation)
}

// relate loads the registrations and relates each one to the specified
// directory, returning the directory with symbolic links resolved and the
// candidates registered by this version of Mutagen and by other versions.
// Registrations by other versions can't be used, but they can still indicate
// that the directory is synchronized, and they're mentioned if no route is
// found.
func relate(directory string) (string, []*candidate, []*candidate, error) {
	registry, err := filesystem.Mutagen(false, registryDirectoryName)
	if err != nil {
		return "", nil, nil, fmt.Errorf("unable to compute command registry directory: %w", err)
	}
	registrations, err := loadRegistrations(registry)
	if err != nil {
		return "", nil, nil, err
	}
	directory = resolveSymbolicLinks(directory)
	var compatible, incompatible []*candidate
	for _, r := range registrations {
		root := resolveSymbolicLinks(r.Root)
		path, contained := relativePath(root, directory)
		var depth int
		if contained {
			depth = len(root)
		}
		c := &candidate{r, contained, depth, path}
		if r.Version == mutagen.Version {
			compatible = append(compatible, c)
		} else {
			incompatible = append(incompatible, c)
		}
	}
	return directory, compatible, incompatible, nil
}

// Dial submits the specified operation through the registered endpoint whose
// root most specifically contains the specified directory, subject to the
// selection. The returned connection is suitable for Execute, Flush, or the
// other functions that complete the operation, and the returned target
// describes the endpoint, which can pin subsequent selections.
func (s *Selection) Dial(ctx context.Context, directory string, operation Operation) (net.Conn, *Target, error) {
	directory, compatible, incompatible, err := relate(directory)
	if err != nil {
		return nil, nil, err
	}

	// Locate the endpoint.
	endpoint, err := s.locate(ctx, directory, compatible, incompatible)
	if err != nil {
		return nil, nil, err
	}
	connection, c := endpoint.connection, endpoint.candidate

	// Refuse operations corresponding to a directory that the session doesn't
	// synchronize (see Selection.Corresponding).
	if s.Target == nil && s.Corresponding && c.contained {
		if excluded := c.Ignores.Excluded(c.path, true); excluded != "" {
			connection.Close()
			return nil, nil, excludedError(directory, c, excluded)
		}
	}

	// Label the sessions that synchronize the directory if there are several
	// and the session wasn't selected (see Target.Overlapping).
	var overlapping []string
	if s.Target == nil && s.Session == "" && c.contained && (s.Default == "" || !matching(s.Default)(c)) {
		all := slices.Concat(compatible, incompatible)
		others, err := synchronizingSessions(ctx, filter(all, func(other *candidate) bool {
			return other.Session != c.Session && other.Ignores.Excluded(other.path, true) == ""
		}))
		if err != nil {
			connection.Close()
			return nil, nil, err
		} else if len(others) > 0 {
			label := labeler(all)
			overlapping = append(overlapping, label(c))
			for _, other := range others {
				overlapping = append(overlapping, label(other))
			}
		}
	}

	// Perform the version handshake and submit the operation.
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	err = mutagen.ClientVersionHandshake(connection)
	if err == nil {
		// A hop that rejects the submission may close the connection before
		// reading the header, so write errors are ignored in favor of the
		// status that the caller reads next.
		writeMessage(connection, &submission{Operation: operation})
	}
	if !stop() {
		return nil, nil, ctx.Err()
	} else if err != nil {
		connection.Close()
		return nil, nil, fmt.Errorf("unable to handshake with session %s: %w", describe(c.Session, c.Name), err)
	}
	return connection, c.target(overlapping), nil
}

// target describes the endpoint of a candidate as a target, with the specified
// overlapping sessions (see Target.Overlapping).
func (c *candidate) target(overlapping []string) *Target {
	return &Target{
		Session:     c.Session,
		Name:        c.Name,
		Alpha:       c.Alpha,
		Root:        c.Root,
		Path:        c.path,
		Contained:   c.contained,
		Overlapping: overlapping,
		Ignores:     c.Ignores,
	}
}

// Locate locates the endpoint through which the selection would submit an
// operation from the specified directory, as Dial does, but without submitting
// one, such as to inspect the session's ignores. Since nothing is submitted,
// ignores that exclude the directory don't prevent it from being located (see
// Selection.Corresponding), and overlapping sessions aren't labeled.
func (s *Selection) Locate(ctx context.Context, directory string) (*Target, error) {
	directory, compatible, incompatible, err := relate(directory)
	if err != nil {
		return nil, err
	}
	endpoint, err := s.locate(ctx, directory, compatible, incompatible)
	if err != nil {
		return nil, err
	}
	endpoint.connection.Close()
	return endpoint.candidate.target(nil), nil
}

// Synchronizing describes the sessions, other than that of the specified
// target, that synchronize content at the specified path, which is a directory
// if directory is true: those whose roots on this system contain the path and
// whose ignores don't exclude it, and, for a directory, those whose roots lie
// beneath it. Writing content that such a session synchronizes, such as that of
// a session whose root is nested inside the target's root, would race with its
// synchronization. Only sessions registered for remote commands on this system
// (by any version of Mutagen) are known, and dead registrations are pruned.
func Synchronizing(ctx context.Context, path string, directory bool, except *Target) ([]string, error) {
	path, compatible, incompatible, err := relate(path)
	if err != nil {
		return nil, err
	}
	others := filter(slices.Concat(compatible, incompatible), func(c *candidate) bool {
		return c.Session != except.Session
	})
	sessions, err := synchronizingSessions(ctx, filter(others, func(c *candidate) bool {
		return c.Ignores.Excluded(c.path, directory) == ""
	}))
	if err != nil {
		return nil, err
	} else if directory {
		nested, err := liveSessions(ctx, filter(others, func(c *candidate) bool {
			_, inside := relativePath(path, resolveSymbolicLinks(c.Root))
			return !c.contained && inside && !slices.ContainsFunc(sessions, func(s *candidate) bool {
				return s.Session == c.Session
			})
		}))
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, nested...)
	}
	result := make([]string, len(sessions))
	for s, c := range sessions {
		result[s] = describe(c.Session, c.Name)
	}
	return result, nil
}

// Sessions lists the endpoints on this system through which operations can be
// submitted, including those registered by other versions of Mutagen, relating
// each to the specified directory: endpoints whose roots contain it come
// first, most specific first and then in the order of preference (see
// precedes), followed by the others, ordered by root. The endpoint that the
// selection locates for the directory, if it locates one, is marked as
// selected, though ignores that exclude the directory would prevent operations
// corresponding to it. Dead registrations are pruned, while registrations
// whose liveness can't be determined are assumed to be live.
func (s *Selection) Sessions(ctx context.Context, directory string) ([]Session, error) {
	directory, compatible, incompatible, err := relate(directory)
	if err != nil {
		return nil, err
	}

	// Probe the newest registration of each endpoint, falling back to older
	// ones as dead registrations are pruned, as probeTier does.
	candidates := slices.Concat(compatible, incompatible)
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Created.After(candidates[j].Created)
	})
	var live []*candidate
	resolved := make(map[string]bool)
	for _, c := range candidates {
		key := fmt.Sprintf("%s/%t", c.Session, c.Alpha)
		if resolved[key] {
			continue
		}
		connection, err := dialRegistration(ctx, c.base+socketExtension)
		if err == nil {
			connection.Close()
		} else if ctx.Err() != nil {
			return nil, ctx.Err()
		} else if isDead(err) {
			c.remove()
			continue
		}
		resolved[key] = true
		live = append(live, c)
	}
	sort.SliceStable(live, func(i, j int) bool {
		if live[i].contained != live[j].contained {
			return live[i].contained
		} else if live[i].depth != live[j].depth {
			return live[i].depth > live[j].depth
		} else if !live[i].contained && live[i].Root != live[j].Root {
			return live[i].Root < live[j].Root
		}
		return precedes(live[i], live[j])
	})

	// Determine the endpoint that the selection locates. Failures to locate one,
	// such as ambiguity, leave every endpoint unselected.
	var selected *candidate
	if endpoint, err := s.locate(ctx, directory, compatible, incompatible); err == nil {
		endpoint.connection.Close()
		selected = endpoint.candidate
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	label := labeler(candidates)
	result := make([]Session, len(live))
	for l, c := range live {
		result[l] = Session{
			Session:   c.Session,
			Name:      c.Name,
			Label:     label(c),
			Alpha:     c.Alpha,
			Root:      c.Root,
			Version:   c.Version,
			Contained: c.contained,
			Excluded:  c.contained && c.Ignores.Excluded(c.path, true) != "",
			Selected:  selected != nil && c.Session == selected.Session && c.Alpha == selected.Alpha,
		}
	}
	return result, nil
}

// locate connects to the endpoint through which to submit an operation from the
// specified directory, given the registrations related to it that are
// compatible with this version of Mutagen and those that aren't.
func (s *Selection) locate(ctx context.Context, directory string, compatible, incompatible []*candidate) (*liveEndpoint, error) {
	// single returns the only live endpoint, or reports ambiguity using the
	// specified function if there are several.
	single := func(live []liveEndpoint, ambiguous func(sessions []string) error) (*liveEndpoint, error) {
		if len(live) > 1 {
			closeEndpoints(live)
			return nil, ambiguous(describeEndpoints(live))
		}
		return &live[0], nil
	}

	// selectedAmbiguity reports ambiguity among the endpoints of the sessions
	// matching a selection, which label describes.
	selectedAmbiguity := func(label string) func([]string) error {
		return func(sessions []string) error {
			if len(sessions) == 1 {
				return fmt.Errorf("both endpoints of session %s are on this system; run from inside one of their roots", sessions[0])
			}
			return fmt.Errorf("multiple sessions match %s (%s); use a session identifier to select one", label, strings.Join(sessions, ", "))
		}
	}

	// first returns the preferred live endpoint among those whose roots
	// contain the directory equally specifically (see precedes), rather than
	// report ambiguity, since Dial labels the other sessions for a warning
	// (see Target.Overlapping).
	first := func(live []liveEndpoint) *liveEndpoint {
		best := 0
		for l := range live {
			if precedes(live[l].candidate, live[best].candidate) {
				best = l
			}
		}
		for l := range live {
			if l != best {
				live[l].connection.Close()
			}
		}
		return &live[best]
	}

	// mismatched explains a failure to locate an endpoint if registrations by
	// other versions of Mutagen would otherwise have been considered, and
	// returns the specified error otherwise.
	mismatched := func(considered []*candidate, err error) error {
		if versions := versions(considered); len(versions) > 0 {
			return fmt.Errorf("matching sessions are registered by Mutagen %s, but this is Mutagen %s; use the remote executable installed with that version",
				strings.Join(versions, ", "), mutagen.Version)
		}
		return err
	}

	// A pinned selection only considers the endpoint that it identifies, which
	// may have reconnected in the meantime, and fails rather than falling back
	// to another endpoint.
	if s.Target != nil {
		pinned := func(c *candidate) bool {
			return c.Session == s.Target.Session && c.Alpha == s.Target.Alpha
		}
		live, err := locate(ctx, filter(compatible, pinned))
		if err != nil {
			return nil, err
		} else if len(live) == 0 {
			return nil, fmt.Errorf("session %s is no longer connected for remote commands on this system (it may have been paused or terminated, or be reconnecting)",
				describe(s.Target.Session, s.Target.Name))
		}
		return single(live, selectedAmbiguity(fmt.Sprintf("%q", s.Target.Session)))
	}

	// An explicitly selected session is used even if its roots don't contain
	// the directory, but not for operations corresponding to the directory
	// while another session's root contains it: they would apply to the root
	// of the selected session's opposite endpoint, as if they had been
	// submitted from outside every root, which is easily mistaken for the
	// directory corresponding to the one inside the other session's root.
	if s.Session != "" {
		selected := matching(s.Session)
		live, err := locate(ctx, filter(compatible, selected, containing))
		if err == nil && len(live) == 0 {
			live, err = locate(ctx, filter(compatible, selected, excluding))
			if err == nil && len(live) > 0 && s.Corresponding {
				others := filter(slices.Concat(compatible, incompatible), func(c *candidate) bool {
					return !selected(c)
				})
				synchronized, err := synchronizing(ctx, others)
				if err != nil || len(synchronized) > 0 {
					closeEndpoints(live)
				}
				if err != nil {
					return nil, err
				} else if len(synchronized) > 0 {
					by := "session " + synchronized[0]
					if len(synchronized) > 1 {
						by = "sessions " + strings.Join(synchronized, ", ")
					}
					return nil, fmt.Errorf("%s is synchronized by %s, not %s; to run the command on the opposite endpoint of %s anyway, use --cwd to choose the directory there, or run remote from outside every synchronized root",
						directory, by, strings.Join(describeEndpoints(live), ", "), s.Session)
				}
			}
		}
		if err != nil {
			return nil, err
		} else if len(live) == 0 {
			return nil, mismatched(filter(incompatible, selected),
				fmt.Errorf("no connected session matching %q has remote commands enabled on this system (remote --sessions lists them)", s.Session))
		}
		return single(live, selectedAmbiguity(fmt.Sprintf("%q", s.Session)))
	}

	// A default session only selects among the sessions whose roots contain
	// the directory, if there are any. If none of them matches it, then the
	// preferred one among the most specific is used, as without a default.
	if s.Default != "" {
		preferred := matching(s.Default)
		label := fmt.Sprintf("%s=%q", SessionVariable, s.Default)
		live, err := locate(ctx, filter(compatible, preferred, containing))
		if err == nil && len(live) > 0 {
			return single(live, selectedAmbiguity(label))
		} else if err == nil {
			live, err = locate(ctx, filter(compatible, containing))
		}
		if err != nil {
			return nil, err
		} else if len(live) > 0 {
			return first(live), nil
		}

		// Outside every root, the default selects a session as an explicit
		// selection does, unless a session registered by another version of
		// Mutagen synchronizes the directory.
		if synchronized, err := synchronizing(ctx, incompatible); err != nil {
			return nil, err
		} else if len(synchronized) == 0 {
			if live, err = locate(ctx, filter(compatible, preferred, excluding)); err != nil {
				return nil, err
			} else if len(live) > 0 {
				return single(live, selectedAmbiguity(label))
			}
		}
		considered := filter(incompatible, func(c *candidate) bool {
			return c.contained || preferred(c)
		})
		return nil, mismatched(considered,
			fmt.Errorf("no connected session with remote commands enabled contains %s or matches %s (remote --sessions lists them)", directory, label))
	}

	// Otherwise, the most specific containing root is used.
	live, err := locate(ctx, filter(compatible, containing))
	if err != nil {
		return nil, err
	} else if len(live) == 0 {
		return nil, mismatched(filter(incompatible, containing),
			fmt.Errorf("no connected session with remote commands enabled contains %s (remote --sessions lists them)", directory))
	}
	return first(live), nil
}
