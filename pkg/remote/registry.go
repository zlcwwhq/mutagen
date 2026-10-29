package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	// scopeDirectoryName is the name of the directory, inside the Mutagen data
	// directory, where synchronization endpoints register the content that they
	// synchronize (see RegisterScope). Scopes are kept apart from command
	// registrations, since versions of remote that predate them would otherwise
	// mistake them for endpoints that accept submissions.
	scopeDirectoryName = "remote-scopes"
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

// record is the on-disk description of a registered endpoint, or of the scope
// of a synchronization endpoint (see RegisterScope).
type record struct {
	// Star, Node, and PeerNode identify a star workspace's submission
	// endpoint (see Endpoint).
	Star     string `json:",omitempty"`
	Node     string `json:",omitempty"`
	PeerNode string `json:",omitempty"`
	// Version is the Mutagen version of the registering process.
	Version string
	// Session is the session identifier.
	Session string
	// Name is the session name, if any. Scopes don't record it, since
	// synchronization endpoints don't know it.
	Name string
	// Root is the normalized synchronization root on this system.
	Root string
	// ExecutionHost and ExecutionRoot describe the opposite endpoint for
	// display (see Endpoint). Scopes and older registrations omit them.
	ExecutionHost string `json:",omitempty"`
	ExecutionRoot string `json:",omitempty"`
	// Alpha indicates whether the registered endpoint is alpha.
	Alpha bool
	// Created is the registration time, used to prefer the newest registration
	// when an endpoint's previous executor has not yet noticed disconnection.
	Created time.Time
	// SessionCreated is the session's creation time (see
	// Endpoint.SessionCreated). Scopes don't record it, since synchronization
	// endpoints don't know it.
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

// loadRegistrations reads all parseable registration records in a registry
// directory.
func loadRegistrations(directory string) ([]*registration, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("unable to read registry: %w", err)
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

// prune removes the registrations in a registry directory whose listeners no
// longer exist, until it's done or the context is cancelled.
func prune(ctx context.Context, directory string) {
	registrations, _ := loadRegistrations(directory)
	for _, r := range registrations {
		if ctx.Err() != nil {
			return
		}
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		connection, err := dialRegistration(probeCtx, r.base+socketExtension)
		cancel()
		if err == nil {
			connection.Close()
		} else if ctx.Err() == nil && isDead(err) {
			r.remove()
		}
	}
}

// register creates a listener for local command submissions from an endpoint
// with the specified normalized root and records it in the command registry,
// once dead registrations are pruned. The record is removed when the listener
// is closed.
func register(endpoint *Endpoint, root string) (net.Listener, error) {
	directory, err := filesystem.Mutagen(true, registryDirectoryName)
	if err != nil {
		return nil, fmt.Errorf("unable to compute command registry directory: %w", err)
	}
	prune(context.Background(), directory)
	return registerIn(directory, &record{
		Star:           endpoint.Star,
		Node:           endpoint.Node,
		PeerNode:       endpoint.PeerNode,
		Session:        endpoint.Session,
		Name:           endpoint.Name,
		Root:           root,
		ExecutionHost:  endpoint.ExecutionHost,
		ExecutionRoot:  endpoint.ExecutionRoot,
		Alpha:          endpoint.Alpha,
		SessionCreated: endpoint.SessionCreated,
		Ignores:        endpoint.Ignores,
	})
}

// registerIn creates a listener in a registry directory and records it there,
// using the specified record, whose version and registration time it sets.
// The record is removed when the listener is closed.
func registerIn(directory string, r *record) (net.Listener, error) {
	// Use short random names because socket paths have tight length limits.
	var identifier [8]byte
	if _, err := rand.Read(identifier[:]); err != nil {
		return nil, fmt.Errorf("unable to generate registration identifier: %w", err)
	}
	base := filepath.Join(directory, hex.EncodeToString(identifier[:]))
	listener, err := ipc.NewListener(base + socketExtension)
	if err != nil {
		return nil, fmt.Errorf("unable to create registration socket: %w", err)
	}

	// Write the record only once the listener exists, so a record whose socket
	// refuses connections is known to be stale. Rename it into place so that
	// readers never observe a partial record.
	r.Version, r.Created = mutagen.Version, time.Now()
	data, err := json.Marshal(r)
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

// pruningScopes indicates whether a scope registration in this process is
// pruning the scope registry, so that registrations don't prune it
// concurrently.
var pruningScopes atomic.Bool

// scopeRegistration is the registration of a synchronization endpoint's scope
// (see RegisterScope).
type scopeRegistration struct {
	// listener is the registration's listener, which accepts connections only
	// to show that the registration is live.
	listener net.Listener
	// cancel cancels the registration's pruning of the scope registry, if any.
	cancel context.CancelFunc
	// pruning tracks pruning the scope registry.
	pruning sync.WaitGroup
	// accepting tracks accepting connections.
	accepting sync.WaitGroup
}

// Close implements io.Closer.Close, removing the registration. Pruning is
// stopped before the record is removed, since pruning reads the registry's
// records, and Windows doesn't remove files while they're open, so removing
// the record while pruning read it would leave it in place until another
// process pruned it. The listener stays open until then, since pruning probes
// this registration too.
func (r *scopeRegistration) Close() error {
	r.cancel()
	r.pruning.Wait()
	err := r.listener.Close()
	r.accepting.Wait()
	return err
}

// RegisterScope registers the scope of a synchronization endpoint on this
// system, identified by the session identifier and whether the endpoint is
// alpha, which is the content beneath its root, normalized as for
// NewLocalChannel, that the session's ignores don't exclude, so that
// Synchronizing knows the session even if it doesn't run remote commands.
// Endpoints keep their scopes registered while they're connected, and closing
// the result removes the registration. Scopes only answer liveness probes,
// without accepting submissions, and so are kept apart from command
// registrations. Dead scope registrations, such as those of processes that
// terminated abruptly, are pruned in the background, so that registration
// doesn't delay synchronization, while Synchronizing prunes those that it
// encounters.
func RegisterScope(session string, alpha bool, root string, ignores *Ignores) (io.Closer, error) {
	root, err := filesystem.Normalize(root)
	if err != nil {
		return nil, fmt.Errorf("unable to normalize synchronization root: %w", err)
	}
	directory, err := filesystem.Mutagen(true, scopeDirectoryName)
	if err != nil {
		return nil, fmt.Errorf("unable to compute scope registry directory: %w", err)
	}
	listener, err := registerIn(directory, &record{Session: session, Root: root, Alpha: alpha, Ignores: ignores})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	registration := &scopeRegistration{listener: listener, cancel: cancel}
	registration.accepting.Add(1)
	go func() {
		defer registration.accepting.Done()
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connection.Close()
		}
	}()
	if pruningScopes.CompareAndSwap(false, true) {
		registration.pruning.Add(1)
		go func() {
			defer registration.pruning.Done()
			defer pruningScopes.Store(false)
			prune(ctx, directory)
		}()
	}
	return registration, nil
}

// Target describes the endpoint from which a command is submitted.
type Target struct {
	// SourceSession and SourceAlpha identify the local registration through
	// which the command is submitted, and they alone pin later submissions
	// (see Selection.Target), so targets built by hand must set them. They
	// never change when the daemon selects another edge of the same node.
	SourceSession string
	SourceAlpha   bool
	// Star identifies a declared star workspace at the submitting registration.
	Star bool
	// Resolved marks a destination selected by the daemon. Revision pins it.
	Resolved bool
	Revision string
	// SourceNode and Node identify the submitting and selected workspaces.
	SourceNode string
	Node       string
	// Session identifies the selected edge for checkout ownership.
	// SourceSession, not this field, pins subsequent local submissions.
	Session string
	// Name is the name of the source session (see SourceSession), if any.
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
	// Ignores are the session's ignores, if known (see Endpoint.Ignores), which
	// determine the paths that the session doesn't synchronize.
	Ignores *Ignores
}

// SessionVariable is the environment variable that formerly selected a
// session, by identifier or name, for the remote executable. Session selectors
// are deprecated in favor of nodes, and the variable is no longer consulted
// here: the remote executable refuses to run while it's set, rather than
// silently ignoring a selection that the user expects to apply.
const SessionVariable = "MUTAGEN_REMOTE_SESSION"

// Selection determines the session, and the endpoint of that session, through
// which Dial submits an operation from a directory.
type Selection struct {
	// Workspace selects the source workspace by node rather than by session:
	// the most specific live workspace containing the directory, which may be
	// registered by several endpoints only if they identify the same node of
	// the same star, as the edges at its center do (see locateWorkspace). Neither
	// creation order nor session names choose among distinct workspaces. It's
	// ignored if Target is non-nil.
	Workspace bool
	// WorkspaceOutside allows a workspace selection from a directory outside
	// all live workspaces, such as for an explicit node selection, in which
	// case the only live workspace on this system is used, and its root
	// applies, if there's exactly one.
	WorkspaceOutside bool
	// Node is resolved by the daemon on the actual operation connection.
	Node string
	// Resolve requests destination resolution, including for an omitted Node.
	Resolve bool
	// Corresponding indicates that the operation applies to the directory
	// corresponding to the submission directory, as commands do unless their
	// working directory is specified explicitly. Dial refuses a submission whose
	// ignores exclude the directory or a parent of it inside the root (see
	// Ignores), since the session doesn't synchronize the corresponding
	// directory. Such a directory is typically the root of a nested session
	// that isn't connected for remote commands on this system, and through
	// which the submission would otherwise have been made.
	Corresponding bool
	// Target, if non-nil, restricts the selection to the local registration
	// that its SourceSession and SourceAlpha identify, which was typically
	// located by a previous submission, so that related submissions can't
	// reach different sessions as sessions connect and disconnect in between.
	// Its Session and Alpha, which may identify another edge selected by the
	// daemon, don't affect the registration. Dial then fails if that
	// registration is no longer live, rather than selecting another, and
	// Workspace is ignored; Corresponding still validates the first submission.
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

// mismatchedVersions explains that the endpoints that a submission would
// otherwise have been made through, which the specified candidates registered
// by other versions of Mutagen describe, can't be used by this version, with
// the specified subject (such as "matching sessions are"). Running the remote
// executable of another version, such as an outdated one that's still on the
// PATH after an upgrade, is the most common cause of failing to locate an
// endpoint, so the wording ("registered by Mutagen X, but this is Mutagen Y")
// is relied on by troubleshooting documentation and should be kept.
func mismatchedVersions(subject string, candidates []*candidate) error {
	return ConnectionError(fmt.Errorf("%s registered by Mutagen %s, but this is Mutagen %s; use the remote executable installed with that version",
		subject, strings.Join(versions(candidates), ", "), mutagen.Version))
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
			return nil, ConnectionError(fmt.Errorf("unable to connect to session %s: %w", describe(c.Session, c.Name), err))
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

// labeler returns a function that labels sessions by name, or by identifier if
// they're unnamed or another session among the candidates shares their name,
// so that labels identify sessions unambiguously in diagnostics.
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
// Ignores.ExcludedInAnyForm).
func excludedError(c *candidate, session, excluded string) error {
	location := fmt.Sprintf("%s is ignored by session %s", c.path, session)
	if excluded != c.path {
		location = fmt.Sprintf("%s is inside %s, which session %s ignores", c.path, excluded, session)
	}
	return fmt.Errorf("%s. If it's another session's root, that session isn't running commands now: don't use --cwd. If it exists separately on each side, --cwd . runs in the other side's own copy.", location)
}

// relate loads the registrations and relates each one to the specified
// directory, returning the directory with symbolic links resolved and the
// candidates registered by this version of Mutagen and by other versions.
// Registrations by other versions can't be used, but they can still indicate
// that the directory is synchronized, and they're mentioned if no route is
// found.
func relate(directory string) (string, []*candidate, []*candidate, error) {
	directory = resolveSymbolicLinks(directory)
	compatible, incompatible, err := relateResolved(registryDirectoryName, directory)
	return directory, compatible, incompatible, err
}

// relateResolved relates the registrations in the registry with the specified
// directory name to the specified path, whose symbolic links have been
// resolved as needed, as relate does, without resolving it further.
func relateResolved(name, directory string) ([]*candidate, []*candidate, error) {
	registry, err := filesystem.Mutagen(false, name)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to compute registry directory: %w", err)
	}
	registrations, err := loadRegistrations(registry)
	if err != nil {
		return nil, nil, err
	}
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
	return compatible, incompatible, nil
}

// Dial submits the specified operation through the registered endpoint whose
// root most specifically contains the specified directory, subject to the
// selection. The returned connection is suitable for Execute, Flush, or the
// other functions that complete the operation, and the returned target
// describes the endpoint, which can pin subsequent selections.
func (s *Selection) Dial(ctx context.Context, directory string, operation Operation) (net.Conn, *Target, error) {
	return s.Submit(ctx, directory, &Submission{Operation: operation})
}

// Submit is Dial with the specified submission header, which can request more
// than the operation (see Submission).
func (s *Selection) Submit(ctx context.Context, directory string, header *Submission) (net.Conn, *Target, error) {
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
	// synchronize (see Selection.Corresponding), or that it might not, since
	// running them anyway could run them in an outdated or missing directory.
	if s.Corresponding && c.contained {
		if excluded := c.Ignores.ExcludedInAnyForm(c.path, true); excluded != "" {
			connection.Close()
			return nil, nil, excludedError(c, labeler(slices.Concat(compatible, incompatible))(c), excluded)
		}
	}

	// Resolve destinations on the operation connection. Pinned targets retain
	// both the original source registration and the selected destination.
	submitted := *header
	header = &submitted
	if s.Resolve || s.Target != nil && s.Target.Resolved {
		header.Resolve = true
		if header.Node == "" {
			header.Node = s.Node
			if header.Node == "" && s.Target != nil && s.Target.Resolved {
				header.Node = s.Target.Node
			}
		}
		if header.Revision == "" && s.Target != nil && s.Target.Resolved {
			header.Revision = s.Target.Revision
		}
	}
	// Perform the version handshake and submit the operation.
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	err = mutagen.ClientVersionHandshake(connection)
	if err == nil {
		// A hop that rejects the submission may close the connection before
		// reading the header, so write errors are ignored in favor of the
		// status that the caller reads next.
		writeMessage(connection, header)
	}
	if !stop() {
		return nil, nil, ctx.Err()
	} else if err != nil {
		connection.Close()
		return nil, nil, ConnectionError(fmt.Errorf("unable to handshake with session %s: %w", describe(c.Session, c.Name), err))
	}
	target := c.target()
	if header.Resolve {
		route, err := receiveResolvedRoute(ctx, connection)
		if err != nil {
			connection.Close()
			return nil, nil, err
		}
		target.Session, target.Alpha = route.Session, route.Alpha
		target.Node, target.SourceNode = route.Node, route.SourceNode
		target.Star, target.Resolved, target.Revision = route.Star, true, route.Revision
		if route.Ignores != nil {
			target.Ignores = route.Ignores
		}
	}
	return connection, target, nil
}

// target describes the endpoint of a candidate as a target. Endpoints of
// implicit pairs, which don't record node names, are named for their roles.
func (c *candidate) target() *Target {
	source, peer := c.Node, c.PeerNode
	if source == "" {
		if c.Alpha {
			source = "alpha"
		} else {
			source = "beta"
		}
	}
	if peer == "" {
		if c.Alpha {
			peer = "beta"
		} else {
			peer = "alpha"
		}
	}
	return &Target{
		SourceSession: c.Session, SourceAlpha: c.Alpha, Star: c.Star != "",
		SourceNode: source,
		Node:       peer,
		Session:    c.Session,
		Name:       c.Name,
		Alpha:      c.Alpha,
		Root:       c.Root,
		Path:       c.path,
		Contained:  c.contained,
		Ignores:    c.Ignores,
	}
}

// Locate locates the endpoint through which the selection would submit an
// operation from the specified directory, as Dial does, but without submitting
// one, such as to inspect the session's ignores. Since nothing is submitted,
// ignores that exclude the directory don't prevent it from being located (see
// Selection.Corresponding).
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
	return endpoint.candidate.target(), nil
}

// Synchronizing describes the sessions that may synchronize content written to
// the specified path, which is a directory if directory is true: those whose
// roots on this system contain the path and whose ignores don't exclude it in
// every form in which scans could see it (see Ignores.ExcludedInEveryForm),
// and, for a directory, those whose roots lie beneath it. Writing content that
// such a session synchronizes, such as that of a session whose root is nested
// inside the writer's root, would race with its synchronization. Sessions are
// related to the path as writes to it land: symbolic links among its parents,
// which must exist, are resolved, since writes follow them, but its final
// component isn't, since writes replace it rather than following it. Both
// endpoints of every session are considered, including those of the session
// through which the content is written, whose ignores apply to the path that
// writes land at, rather than the one given.
//
// Sessions are known from the scopes that their synchronization endpoints on
// this system register while they're connected (see RegisterScope), whether
// or not the sessions run remote commands, and from command registrations, by
// any version of Mutagen. Sessions whose endpoints on this system aren't
// connected, such as paused or reconnecting sessions, aren't known, and
// neither are sessions run by versions of Mutagen that predate scopes, unless
// they register endpoints for remote commands. Sessions are described by name
// if a command registration of the session provides one, since scopes don't
// record names, and by identifier otherwise. Dead registrations are pruned.
func Synchronizing(ctx context.Context, path string, directory bool) ([]string, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("unable to resolve symbolic links: %w", err)
	}
	path = filepath.Join(parent, filepath.Base(path))
	compatible, incompatible, err := relateResolved(registryDirectoryName, path)
	if err != nil {
		return nil, err
	}
	registered := slices.Concat(compatible, incompatible)
	compatible, incompatible, err = relateResolved(scopeDirectoryName, path)
	if err != nil {
		return nil, err
	}
	scopes := slices.Concat(compatible, incompatible)

	// Scopes don't record session names or creation times, so take them from
	// command registrations of the same sessions, if any, to describe and
	// order the sessions as Dial does. Registrations of the same session are
	// then filtered alike and only described once.
	for _, s := range scopes {
		if r := slices.IndexFunc(registered, func(c *candidate) bool { return c.Session == s.Session }); r >= 0 {
			s.Name, s.SessionCreated = registered[r].Name, registered[r].SessionCreated
		}
	}
	all := slices.Concat(registered, scopes)
	sessions, err := synchronizingSessions(ctx, filter(all, func(c *candidate) bool {
		return c.Ignores.ExcludedInEveryForm(c.path, directory) == ""
	}))
	if err != nil {
		return nil, err
	} else if directory {
		nested, err := liveSessions(ctx, filter(all, func(c *candidate) bool {
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

// locateWorkspace selects the source workspace for a workspace selection (see
// Selection.Workspace), given the registrations related to the submission
// directory that are compatible with this version of Mutagen and those that
// aren't. The most specific live workspace containing the directory is used,
// or, if outside is true and no live workspace contains it, the only live
// workspace on this system. Several live registrations are only accepted when
// they identify the same node of the same star, as the endpoints of a star's
// edges at its center do; anything else is reported as ambiguous rather than
// resolved by creation order.
//
// Registrations whose processes no longer listen are pruned and otherwise
// ignored, exactly as if they had been removed cleanly, as the registrations
// of paused or reconnecting sessions are. A more specific workspace that isn't
// connected therefore can't be told apart from one that doesn't exist, and an
// enclosing workspace is used instead. For operations corresponding to the
// directory, the enclosing workspace's ignores, which typically exclude a
// nested workspace's root, then refuse the submission (see
// Selection.Corresponding).
//
// Live registrations by other versions of Mutagen can't be used, but they
// aren't masked either: if one contains the directory at least as specifically
// as the selected workspace, or no compatible workspace is found, the version
// mismatch is reported (see mismatchedVersions), since the submission was
// probably meant for it.
func locateWorkspace(ctx context.Context, compatible, incompatible []*candidate, outside bool) (*liveEndpoint, error) {
	// Determine the live registrations by other versions of Mutagen whose roots
	// contain the directory, most specific first, pruning dead ones, so that a
	// stale record can't block selection.
	others, err := synchronizingSessions(ctx, incompatible)
	if err != nil {
		return nil, err
	}

	// Connect to the most specific live workspaces containing the directory,
	// or, if permitted and none does, to all live workspaces. Dead
	// registrations are pruned as tiers are probed, and the next tier is used
	// if a tier has no live registrations (see locate).
	live, err := locate(ctx, filter(compatible, func(c *candidate) bool {
		return c.contained || outside
	}))
	if err != nil {
		return nil, err
	} else if len(live) == 0 {
		subject := "the workspaces containing this directory are"
		if len(others) == 0 && outside {
			subject = "the workspaces on this system are"
			if others, err = liveSessions(ctx, filter(incompatible, excluding)); err != nil {
				return nil, err
			}
		}
		if len(others) > 0 {
			return nil, mismatchedVersions(subject, others)
		}
		return nil, ConnectionError(errors.New("no connected node contains the current directory; run from a workspace root or its subdirectory"))
	}
	chosen := live[0]

	// Refuse to mask a live workspace registered by another version of Mutagen
	// that contains the directory at least as specifically. Since the other
	// registrations are ordered most specific first, the first one determines
	// whether they're all equally specific.
	var masking []*candidate
	for _, c := range others {
		if c.depth >= chosen.candidate.depth {
			masking = append(masking, c)
		}
	}
	if len(masking) > 0 {
		closeEndpoints(live)
		subject := "a more specific workspace containing this directory is"
		if !chosen.candidate.contained {
			subject = "a workspace containing this directory is"
		} else if masking[0].depth == chosen.candidate.depth {
			subject = "another workspace with the same root is"
		}
		return nil, mismatchedVersions(subject, masking)
	}

	// Several live registrations must identify the same node: a star's edges
	// share its center's root, while distinct pairs or stars sharing a root
	// are ambiguous.
	for _, endpoint := range live[1:] {
		first, other := chosen.candidate, endpoint.candidate
		same := first.Star != "" && first.Star == other.Star && first.Node == other.Node
		if first.Star == "" && other.Star == "" {
			same = first.Session == other.Session && first.Alpha == other.Alpha
		}
		if !same {
			closeEndpoints(live)
			return nil, errors.New("ambiguous source node; declare the workspaces sharing this root as one star, or run from an unambiguous workspace")
		}
	}
	for _, endpoint := range live[1:] {
		endpoint.connection.Close()
	}
	return &chosen, nil
}

// locate connects to the endpoint through which to submit an operation from the
// specified directory, given the registrations related to it that are
// compatible with this version of Mutagen and those that aren't.
func (s *Selection) locate(ctx context.Context, directory string, compatible, incompatible []*candidate) (*liveEndpoint, error) {
	// A pinned selection only considers the source registration that it
	// identifies, which may have reconnected in the meantime, and fails rather
	// than falling back to another endpoint. The daemon-selected edge (Session
	// and Alpha) never chooses the registration, even if the source is unset.
	if s.Target != nil {
		source := s.Target.SourceSession
		if source == "" {
			return nil, errors.New("pinned target doesn't identify a source registration")
		}
		pinned := func(c *candidate) bool {
			return c.Session == source && c.Alpha == s.Target.SourceAlpha
		}
		live, err := locate(ctx, filter(compatible, pinned))
		if err != nil {
			return nil, err
		} else if len(live) == 0 {
			endpoint := "beta endpoint"
			if s.Target.SourceAlpha {
				endpoint = "alpha endpoint"
			}
			if s.Target.Star && s.Target.SourceNode != "" {
				endpoint = fmt.Sprintf("%s (node %s)", endpoint, s.Target.SourceNode)
			}
			return nil, ConnectionError(fmt.Errorf("the %s of session %s is no longer connected for remote commands on this system (it may have been paused or terminated, or be reconnecting)",
				endpoint, describe(source, s.Target.Name)))
		}
		return &live[0], nil
	}

	// A workspace selection is resolved by node (see locateWorkspace).
	if s.Workspace {
		return locateWorkspace(ctx, compatible, incompatible, s.WorkspaceOutside)
	}

	// Otherwise, the most specific containing root is used, and among equally
	// specific roots, the preferred session (see precedes).
	live, err := locate(ctx, filter(compatible, containing))
	if err != nil {
		return nil, err
	} else if len(live) == 0 {
		if mismatched := filter(incompatible, containing); len(mismatched) > 0 {
			return nil, mismatchedVersions("matching sessions are", mismatched)
		}
		return nil, ConnectionError(fmt.Errorf("no connected session with remote commands enabled contains %s", directory))
	}
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
	return &live[best], nil
}
