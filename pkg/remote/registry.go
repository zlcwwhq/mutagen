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
		Ignores: endpoint.Ignores,
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
		connection, err := ipc.DialContext(ctx, c.base+socketExtension)
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
// registrations of the same endpoint are ranked by age (see probeTier). More
// than one live endpoint indicates ambiguity, which the caller reports.
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

// synchronizing describes the sessions with live registrations among
// candidates whose roots contain the submission directory, most specific
// first. Dead registrations are pruned, while registrations whose liveness
// can't be determined are assumed to be live. Registrations are only probed
// for liveness, without a version handshake, so registrations by other
// versions of Mutagen are included.
func synchronizing(ctx context.Context, candidates []*candidate) ([]string, error) {
	candidates = filter(candidates, containing)
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].depth > candidates[j].depth
	})
	var result []string
	described := make(map[string]bool)
	for _, c := range candidates {
		if described[c.Session] {
			continue
		}
		connection, err := ipc.DialContext(ctx, c.base+socketExtension)
		if err == nil {
			connection.Close()
		} else if ctx.Err() != nil {
			return nil, ctx.Err()
		} else if isDead(err) {
			c.remove()
			continue
		}
		described[c.Session] = true
		result = append(result, describe(c.Session, c.Name))
	}
	return result, nil
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
// Ignores.excluded).
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

// Dial submits the specified operation through the registered endpoint whose
// root most specifically contains the specified directory, subject to the
// selection. The returned connection is suitable for Execute, Flush, or the
// other functions that complete the operation, and the returned target
// describes the endpoint, which can pin subsequent selections.
func (s *Selection) Dial(ctx context.Context, directory string, operation Operation) (net.Conn, *Target, error) {
	registry, err := filesystem.Mutagen(false, registryDirectoryName)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to compute command registry directory: %w", err)
	}
	registrations, err := loadRegistrations(registry)
	if err != nil {
		return nil, nil, err
	}

	// Relate each registration to the directory. Registrations by other
	// versions of Mutagen can't be used, but they can still indicate that the
	// directory is synchronized, and they're mentioned if no route is found.
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

	// Locate the endpoint.
	endpoint, err := s.locate(ctx, directory, compatible, incompatible)
	if err != nil {
		return nil, nil, err
	}
	connection, c := endpoint.connection, endpoint.candidate

	// Refuse operations corresponding to a directory that the session doesn't
	// synchronize (see Selection.Corresponding).
	if s.Target == nil && s.Corresponding && c.contained {
		if excluded := c.Ignores.excluded(c.path); excluded != "" {
			connection.Close()
			return nil, nil, excludedError(directory, c, excluded)
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
	return connection, &Target{
		Session:   c.Session,
		Name:      c.Name,
		Alpha:     c.Alpha,
		Root:      c.Root,
		Path:      c.path,
		Contained: c.contained,
	}, nil
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

	// containedAmbiguity reports ambiguity among sessions whose roots contain
	// the directory equally specifically.
	containedAmbiguity := func(sessions []string) error {
		return fmt.Errorf("multiple sessions contain %s (%s); use --session to select one", directory, strings.Join(sessions, ", "))
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
				fmt.Errorf("no connected session matching %q has remote commands enabled on this system", s.Session))
		}
		return single(live, selectedAmbiguity(fmt.Sprintf("%q", s.Session)))
	}

	// A default session only selects among the sessions whose roots contain
	// the directory, if there are any. Ambiguity among containing sessions
	// that don't match it is still reported, rather than resolved by it.
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
			return single(live, containedAmbiguity)
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
			fmt.Errorf("no connected session with remote commands enabled contains %s or matches %s", directory, label))
	}

	// Otherwise, the most specific containing root is used.
	live, err := locate(ctx, filter(compatible, containing))
	if err != nil {
		return nil, err
	} else if len(live) == 0 {
		return nil, mismatched(filter(incompatible, containing),
			fmt.Errorf("no connected session with remote commands enabled contains %s", directory))
	}
	return single(live, containedAmbiguity)
}
