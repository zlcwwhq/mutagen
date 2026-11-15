package remote

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/encoding"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// testingPackReceiver is a synchronization.RepositoryReceiver that records what
// it receives.
type testingPackReceiver struct {
	// data is the data received.
	data []byte
	// sizes are the sizes of the parts received.
	sizes []int
	// finished indicates that the pack has been finished.
	finished bool
	// problem is the problem with which the pack was finished.
	problem string
}

// Receive implements synchronization.RepositoryReceiver.Receive.
func (r *testingPackReceiver) Receive(data []byte) error {
	if r.finished {
		return errors.New("pack already finished")
	}
	r.data = append(r.data, data...)
	r.sizes = append(r.sizes, len(data))
	return nil
}

// Finish implements synchronization.RepositoryReceiver.Finish.
func (r *testingPackReceiver) Finish(problem string) error {
	if r.finished {
		return errors.New("pack already finished")
	}
	r.finished, r.problem = true, problem
	return nil
}

// testingRepositoryEndpoint is a synchronization.Endpoint that only implements
// the repository methods.
type testingRepositoryEndpoint struct {
	synchronization.Endpoint
	// haves are the commits that StageRepository returns.
	haves [][]byte
	// receiver is the receiver that StageRepository returns, if any.
	receiver *testingPackReceiver
	// pack is the pack that SupplyRepository supplies.
	pack []byte
	// problem is the problem with which SupplyRepository fails to produce the
	// pack, if any.
	problem string
	// failure is the error with which SupplyRepository and
	// TransitionRepository fail, if any, without finishing the pack.
	failure string
	// commit and supplyHaves are the arguments of the last SupplyRepository.
	commit      []byte
	supplyHaves [][]byte
	// actions are the actions that StageRepository and TransitionRepository
	// have received.
	actions []*core.RepositoryAction
}

// StageRepository implements synchronization.Endpoint.StageRepository.
func (e *testingRepositoryEndpoint) StageRepository(_ context.Context, action *core.RepositoryAction) ([][]byte, synchronization.RepositoryReceiver, error) {
	e.actions = append(e.actions, action)
	if e.receiver == nil {
		return e.haves, nil, nil
	}
	return e.haves, e.receiver, nil
}

// SupplyRepository implements synchronization.Endpoint.SupplyRepository.
func (e *testingRepositoryEndpoint) SupplyRepository(_ context.Context, commit []byte, haves [][]byte, receiver synchronization.RepositoryReceiver) (*core.Problem, error) {
	e.commit, e.supplyHaves = commit, haves
	if e.failure != "" {
		return nil, errors.New(e.failure)
	} else if e.problem != "" {
		return &core.Problem{Path: ".git", Error: e.problem}, receiver.Finish(e.problem)
	} else if err := receiver.Receive(e.pack); err != nil {
		return nil, err
	}
	return nil, receiver.Finish("")
}

// TransitionRepository implements
// synchronization.Endpoint.TransitionRepository.
func (e *testingRepositoryEndpoint) TransitionRepository(_ context.Context, action *core.RepositoryAction) (*core.RepositoryResult, error) {
	e.actions = append(e.actions, action)
	if e.failure != "" {
		return nil, errors.New(e.failure)
	}
	return &core.RepositoryResult{Outcome: core.RepositoryOutcome_Moved}, nil
}

// newTestingRepositoryClient connects a client to a server for the specified
// endpoint over an in-memory connection. It returns the client and a channel
// that receives the server's error once it terminates.
func newTestingRepositoryClient(t *testing.T, endpoint synchronization.Endpoint) (*endpointClient, <-chan error) {
	clientConnection, serverConnection := net.Pipe()
	clientOutbound, serverOutbound := bufio.NewWriter(clientConnection), bufio.NewWriter(serverConnection)
	client := &endpointClient{
		closer:  clientConnection,
		flusher: clientOutbound,
		encoder: encoding.NewProtobufEncoder(clientOutbound),
		decoder: encoding.NewProtobufDecoder(bufio.NewReader(clientConnection)),
	}
	server := &endpointServer{
		endpoint: endpoint,
		flusher:  serverOutbound,
		encoder:  encoding.NewProtobufEncoder(serverOutbound),
		decoder:  encoding.NewProtobufDecoder(bufio.NewReader(serverConnection)),
	}
	done := make(chan error, 1)
	go func() {
		done <- server.serve()
		serverConnection.Close()
	}()
	t.Cleanup(func() {
		clientConnection.Close()
	})
	return client, done
}

// testingPack creates pack data of the specified size, with content that
// varies, so that misplaced chunks are detected.
func testingPack(size int) []byte {
	pack := make([]byte, size)
	for i := range pack {
		pack[i] = byte(i % 251)
	}
	return pack
}

// testingAction is a valid repository action.
var testingAction = &core.RepositoryAction{
	Old:    &core.Repository{Commit: bytes.Repeat([]byte{1}, 20), Branch: "main"},
	Target: &core.Repository{Commit: bytes.Repeat([]byte{2}, 20), Branch: "main"},
	Policy: core.RepositoryPolicy_FastForward,
}

// TestRepositoryStaging tests that a pack sent to a remote endpoint's receiver
// arrives intact, in chunks of bounded size, before the following request.
func TestRepositoryStaging(t *testing.T) {
	ctx := context.Background()
	endpoint := &testingRepositoryEndpoint{haves: [][]byte{testingAction.Old.Commit}, receiver: &testingPackReceiver{}}
	client, _ := newTestingRepositoryClient(t, endpoint)

	// Stage and send a pack of several megabytes in parts of various sizes.
	haves, receiver, err := client.StageRepository(ctx, testingAction)
	if err != nil {
		t.Fatal("unable to stage:", err)
	} else if receiver == nil {
		t.Fatal("no receiver")
	} else if !slices.EqualFunc(haves, endpoint.haves, bytes.Equal) {
		t.Error("incorrect commits:", haves)
	}
	pack := testingPack(3*1024*1024 + 17)
	for _, part := range [][]byte{pack[:5], pack[5 : 1024*1024], pack[1024*1024:]} {
		if err := receiver.Receive(part); err != nil {
			t.Fatal("unable to send pack:", err)
		}
	}
	if err := receiver.Finish(""); err != nil {
		t.Fatal("unable to finish pack:", err)
	}

	// Apply the action, which the server serves once it's received the pack.
	if result, err := client.TransitionRepository(ctx, testingAction); err != nil {
		t.Fatal("unable to apply action:", err)
	} else if result.Outcome != core.RepositoryOutcome_Moved {
		t.Error("incorrect result:", result)
	}
	if !bytes.Equal(endpoint.receiver.data, pack) || !endpoint.receiver.finished || endpoint.receiver.problem != "" {
		t.Error("pack not received intact")
	} else if slices.Max(endpoint.receiver.sizes) > maximumRepositoryChunkSize {
		t.Error("chunk too large:", slices.Max(endpoint.receiver.sizes))
	}
	if len(endpoint.actions) != 2 || !proto.Equal(endpoint.actions[0], testingAction) || !proto.Equal(endpoint.actions[1], testingAction) {
		t.Error("incorrect actions:", endpoint.actions)
	}

	// An endpoint that has the objects doesn't need a pack.
	endpoint.receiver = nil
	if haves, receiver, err := client.StageRepository(ctx, testingAction); err != nil {
		t.Fatal("unable to stage:", err)
	} else if receiver != nil || len(haves) != 1 {
		t.Error("unexpected staging results:", haves, receiver)
	}
}

// TestRepositorySupply tests that a pack supplied by a remote endpoint arrives
// intact, that a problem producing it is reported without terminating the
// connection, and that other failures are errors.
func TestRepositorySupply(t *testing.T) {
	ctx := context.Background()
	endpoint := &testingRepositoryEndpoint{pack: testingPack(2*1024*1024 + 3)}
	client, done := newTestingRepositoryClient(t, endpoint)
	commit, haves := testingAction.Target.Commit, [][]byte{testingAction.Old.Commit}

	// Supply a pack.
	receiver := &testingPackReceiver{}
	if problem, err := client.SupplyRepository(ctx, commit, haves, receiver); err != nil {
		t.Fatal("unable to supply:", err)
	} else if problem != nil {
		t.Error("unexpected problem:", problem)
	}
	if !bytes.Equal(receiver.data, endpoint.pack) || !receiver.finished || receiver.problem != "" {
		t.Error("pack not received intact")
	} else if slices.Max(receiver.sizes) > maximumRepositoryChunkSize {
		t.Error("chunk too large:", slices.Max(receiver.sizes))
	}
	if !bytes.Equal(endpoint.commit, commit) || !slices.EqualFunc(endpoint.supplyHaves, haves, bytes.Equal) {
		t.Error("incorrect supply arguments")
	}

	// A problem producing the pack is reported, and the connection remains
	// usable.
	endpoint.problem = "the repository is shallow"
	receiver = &testingPackReceiver{}
	if problem, err := client.SupplyRepository(ctx, commit, haves, receiver); err != nil {
		t.Fatal("unable to supply:", err)
	} else if problem == nil || problem.Path != ".git" || problem.Error != endpoint.problem {
		t.Error("incorrect problem:", problem)
	}
	if len(receiver.data) != 0 || !receiver.finished || receiver.problem != endpoint.problem {
		t.Error("pack not finished with problem")
	}
	if _, err := client.TransitionRepository(ctx, testingAction); err != nil {
		t.Fatal("connection unusable after problem:", err)
	}

	// An error applying an action is reported without terminating the
	// connection.
	endpoint.failure = "unable to run Git"
	if _, err := client.TransitionRepository(ctx, testingAction); err == nil || !strings.Contains(err.Error(), endpoint.failure) {
		t.Error("incorrect transition error:", err)
	}

	// An error supplying the pack finishes the receiver, is reported, and
	// terminates the server.
	receiver = &testingPackReceiver{}
	if _, err := client.SupplyRepository(ctx, commit, haves, receiver); err == nil || !strings.Contains(err.Error(), endpoint.failure) {
		t.Error("incorrect supply error:", err)
	}
	if !receiver.finished || receiver.problem == "" {
		t.Error("pack not finished after error")
	}
	if err := <-done; err == nil {
		t.Error("server not terminated")
	}
}

// TestRepositoryMessageValidation tests the validation of the messages that
// transmit repository operations.
func TestRepositoryMessageValidation(t *testing.T) {
	commit := bytes.Repeat([]byte{1}, 20)
	tests := []struct {
		description string
		message     interface{ ensureValid() error }
		valid       bool
	}{
		{"staging request", &StageRepositoryRequest{Action: testingAction}, true},
		{"staging request without action", &StageRepositoryRequest{}, false},
		{"conditional attachment", &StageRepositoryRequest{Action: &core.RepositoryAction{
			Target: testingAction.Target, Policy: core.RepositoryPolicy_FastForward,
		}}, false},
		{"staging response", &StageRepositoryResponse{Haves: [][]byte{commit}, Receive: true}, true},
		{"staging response with invalid commit", &StageRepositoryResponse{Haves: [][]byte{{1}}}, false},
		{"staging response with error and pack", &StageRepositoryResponse{Error: "error", Receive: true}, false},
		{"supply request", &SupplyRepositoryRequest{Commit: commit, Haves: [][]byte{commit}}, true},
		{"supply request without commit", &SupplyRepositoryRequest{}, false},
		{"supply request with invalid commit", &SupplyRepositoryRequest{Commit: commit, Haves: [][]byte{{1}}}, false},
		{"chunk", &RepositoryChunk{Data: make([]byte, maximumRepositoryChunkSize)}, true},
		{"oversized chunk", &RepositoryChunk{Data: make([]byte, maximumRepositoryChunkSize+1)}, false},
		{"end of pack", &RepositoryChunk{Done: true}, true},
		{"end of pack with problem", &RepositoryChunk{Done: true, Problem: "problem"}, true},
		{"end of pack with data", &RepositoryChunk{Done: true, Data: commit}, false},
		{"problem before end of pack", &RepositoryChunk{Problem: "problem"}, false},
		{"supply response", &SupplyRepositoryResponse{}, true},
		{"supply response with problem", &SupplyRepositoryResponse{Problem: &core.Problem{Path: ".git", Error: "problem"}}, true},
		{"supply response with invalid problem", &SupplyRepositoryResponse{Problem: &core.Problem{Path: ".git"}}, false},
		{"transition request", &TransitionRepositoryRequest{Action: testingAction}, true},
		{"transition request without action", &TransitionRepositoryRequest{}, false},
		{"transition response", &TransitionRepositoryResponse{Result: &core.RepositoryResult{Outcome: core.RepositoryOutcome_Ahead}}, true},
		{"transition response without result", &TransitionRepositoryResponse{}, false},
		{"transition response with error", &TransitionRepositoryResponse{Error: "error"}, true},
		{"transition response with error and result", &TransitionRepositoryResponse{
			Error: "error", Result: &core.RepositoryResult{Outcome: core.RepositoryOutcome_Moved},
		}, false},
		{"transition response with failure without problem", &TransitionRepositoryResponse{Result: &core.RepositoryResult{}}, false},
		{"endpoint request", &EndpointRequest{StageRepository: &StageRepositoryRequest{}}, true},
		{"endpoint request with two requests", &EndpointRequest{
			SupplyRepository:     &SupplyRepositoryRequest{},
			TransitionRepository: &TransitionRepositoryRequest{},
		}, false},
	}
	for _, test := range tests {
		if err := test.message.ensureValid(); (err == nil) != test.valid {
			t.Errorf("%s: incorrect validity: %v", test.description, err)
		}
	}
}
