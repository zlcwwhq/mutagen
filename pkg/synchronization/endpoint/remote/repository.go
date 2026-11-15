package remote

import (
	"context"
	"errors"
	"fmt"

	"github.com/mutagen-io/mutagen/pkg/encoding"
	streampkg "github.com/mutagen-io/mutagen/pkg/stream"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// protobufRepositoryEncoder implements synchronization.RepositoryReceiver by
// transmitting a pack as RepositoryChunk messages.
type protobufRepositoryEncoder struct {
	// encoder is the underlying Protocol Buffers encoder.
	encoder *encoding.ProtobufEncoder
	// flusher flushes the underlying stream.
	flusher streampkg.Flusher
	// finished indicates that the end of the pack has been transmitted.
	finished bool
	// error stores any previously encountered transmission error.
	error error
}

// Receive implements synchronization.RepositoryReceiver.Receive. It splits the
// data into chunks of bounded size.
func (e *protobufRepositoryEncoder) Receive(data []byte) error {
	// Check for previous errors and for the end of the pack.
	if e.error != nil {
		return fmt.Errorf("previous error encountered: %w", e.error)
	} else if e.finished {
		return errors.New("pack already finished")
	}

	// Transmit the data. It's flushed with the end of the pack.
	for len(data) > 0 {
		size := min(len(data), maximumRepositoryChunkSize)
		if e.error = e.encoder.Encode(&RepositoryChunk{Data: data[:size]}); e.error != nil {
			return e.error
		}
		data = data[size:]
	}
	return nil
}

// Finish implements synchronization.RepositoryReceiver.Finish.
func (e *protobufRepositoryEncoder) Finish(problem string) error {
	// Check for previous errors and for the end of the pack.
	if e.error != nil {
		return fmt.Errorf("previous error encountered: %w", e.error)
	} else if e.finished {
		return errors.New("pack already finished")
	}

	// Transmit the end of the pack.
	e.finished = true
	if e.error = e.encoder.Encode(&RepositoryChunk{Done: true, Problem: problem}); e.error != nil {
		return e.error
	} else if e.error = e.flusher.Flush(); e.error != nil {
		return fmt.Errorf("unable to flush pack: %w", e.error)
	}
	return nil
}

// receiveRepositoryChunks receives a pack as RepositoryChunk messages and
// forwards it to a receiver, which it finishes. It fails once the context is
// cancelled, like rsync.NewPreemptableReceiver.
func receiveRepositoryChunks(ctx context.Context, decoder *encoding.ProtobufDecoder, receiver synchronization.RepositoryReceiver) error {
	for {
		chunk := &RepositoryChunk{}
		if err := decoder.Decode(chunk); err != nil {
			return fmt.Errorf("unable to receive pack chunk: %w", err)
		} else if err = chunk.ensureValid(); err != nil {
			return fmt.Errorf("invalid pack chunk: %w", err)
		} else if chunk.Done {
			return receiver.Finish(chunk.Problem)
		} else if err = ctx.Err(); err != nil {
			return fmt.Errorf("pack reception cancelled: %w", err)
		} else if err = receiver.Receive(chunk.Data); err != nil {
			return err
		}
	}
}

// StageRepository implements synchronization.Endpoint.StageRepository.
func (c *endpointClient) StageRepository(_ context.Context, action *core.RepositoryAction) ([][]byte, synchronization.RepositoryReceiver, error) {
	// Create and send the repository staging request.
	request := &EndpointRequest{StageRepository: &StageRepositoryRequest{Action: action}}
	if err := c.encodeAndFlush(request); err != nil {
		return nil, nil, fmt.Errorf("unable to send repository staging request: %w", err)
	}

	// Receive the response and check for remote errors.
	response := &StageRepositoryResponse{}
	if err := c.decoder.Decode(response); err != nil {
		return nil, nil, fmt.Errorf("unable to receive repository staging response: %w", err)
	} else if err = response.ensureValid(); err != nil {
		return nil, nil, fmt.Errorf("invalid repository staging response: %w", err)
	} else if response.Error != "" {
		return nil, nil, fmt.Errorf("remote error: %s", response.Error)
	}

	// If the remote needs a pack, then create a receiver that transmits it.
	if !response.Receive {
		return response.Haves, nil, nil
	}
	return response.Haves, &protobufRepositoryEncoder{encoder: c.encoder, flusher: c.flusher}, nil
}

// SupplyRepository implements synchronization.Endpoint.SupplyRepository.
func (c *endpointClient) SupplyRepository(ctx context.Context, commit []byte, haves [][]byte, receiver synchronization.RepositoryReceiver) (*core.Problem, error) {
	// Create and send the repository supply request.
	request := &EndpointRequest{SupplyRepository: &SupplyRepositoryRequest{Commit: commit, Haves: haves}}
	if err := c.encodeAndFlush(request); err != nil {
		return nil, fmt.Errorf("unable to send repository supply request: %w", err)
	}

	// Forward the pack to the receiver. The remote ends it even if it can't
	// produce it.
	if err := receiveRepositoryChunks(ctx, c.decoder, receiver); err != nil {
		return nil, fmt.Errorf("unable to forward pack: %w", err)
	}

	// Receive the response and check for remote errors.
	response := &SupplyRepositoryResponse{}
	if err := c.decoder.Decode(response); err != nil {
		return nil, fmt.Errorf("unable to receive repository supply response: %w", err)
	} else if err = response.ensureValid(); err != nil {
		return nil, fmt.Errorf("invalid repository supply response: %w", err)
	} else if response.Error != "" {
		return nil, fmt.Errorf("remote error: %s", response.Error)
	}

	// Success.
	return response.Problem, nil
}

// TransitionRepository implements
// synchronization.Endpoint.TransitionRepository. Unlike Transition, it can't
// be cancelled, since applying an action involves little work.
func (c *endpointClient) TransitionRepository(_ context.Context, action *core.RepositoryAction) (*core.RepositoryResult, error) {
	// Create and send the repository transition request.
	request := &EndpointRequest{TransitionRepository: &TransitionRepositoryRequest{Action: action}}
	if err := c.encodeAndFlush(request); err != nil {
		return nil, fmt.Errorf("unable to send repository transition request: %w", err)
	}

	// Receive the response and check for remote errors.
	response := &TransitionRepositoryResponse{}
	if err := c.decoder.Decode(response); err != nil {
		return nil, fmt.Errorf("unable to receive repository transition response: %w", err)
	} else if err = response.ensureValid(); err != nil {
		return nil, fmt.Errorf("invalid repository transition response: %w", err)
	} else if response.Error != "" {
		return nil, fmt.Errorf("remote error: %s", response.Error)
	}

	// Success.
	return response.Result, nil
}

// serveStageRepository serves a repository staging request.
func (s *endpointServer) serveStageRepository(request *StageRepositoryRequest) error {
	// Ensure the request is valid.
	if err := request.ensureValid(); err != nil {
		return fmt.Errorf("invalid repository staging request: %w", err)
	}

	// Begin staging.
	haves, receiver, err := s.endpoint.StageRepository(context.Background(), request.Action)
	if err != nil {
		s.encodeAndFlush(&StageRepositoryResponse{Error: err.Error()})
		return fmt.Errorf("unable to begin repository staging: %w", err)
	}

	// Send the response.
	response := &StageRepositoryResponse{Haves: haves, Receive: receiver != nil}
	if err = s.encodeAndFlush(response); err != nil {
		return fmt.Errorf("unable to send repository staging response: %w", err)
	}

	// If a pack is needed, then the client sends it next, which we forward to
	// the receiver.
	if receiver != nil {
		if err = receiveRepositoryChunks(context.Background(), s.decoder, receiver); err != nil {
			return fmt.Errorf("unable to receive pack: %w", err)
		}
	}

	// Success.
	return nil
}

// serveSupplyRepository serves a repository supply request.
func (s *endpointServer) serveSupplyRepository(request *SupplyRepositoryRequest) error {
	// Ensure the request is valid.
	if err := request.ensureValid(); err != nil {
		return fmt.Errorf("invalid repository supply request: %w", err)
	}

	// Supply the pack.
	encoder := &protobufRepositoryEncoder{encoder: s.encoder, flusher: s.flusher}
	problem, err := s.endpoint.SupplyRepository(context.Background(), request.Commit, request.Haves, encoder)
	if encoder.error != nil {
		return fmt.Errorf("unable to transmit pack: %w", encoder.error)
	}

	// If the endpoint failed without ending the pack, then end it, since the
	// client expects its end before the response.
	if !encoder.finished {
		if err == nil {
			err = errors.New("pack not finished")
		}
		if finishErr := encoder.Finish(err.Error()); finishErr != nil {
			return fmt.Errorf("unable to transmit end of pack: %w", finishErr)
		}
	}

	// Send the response. Like a failure to supply files, a failure to supply
	// the pack (other than a problem producing it) terminates the server.
	response := &SupplyRepositoryResponse{Problem: problem}
	if err != nil {
		response = &SupplyRepositoryResponse{Error: err.Error()}
	}
	if sendErr := s.encodeAndFlush(response); sendErr != nil {
		return fmt.Errorf("unable to send repository supply response: %w", sendErr)
	} else if err != nil {
		return fmt.Errorf("unable to supply pack: %w", err)
	}

	// Success.
	return nil
}

// serveTransitionRepository serves a repository transition request.
func (s *endpointServer) serveTransitionRepository(request *TransitionRepositoryRequest) error {
	// Ensure the request is valid.
	if err := request.ensureValid(); err != nil {
		return fmt.Errorf("invalid repository transition request: %w", err)
	}

	// Apply the action and send the response.
	response := &TransitionRepositoryResponse{}
	if result, err := s.endpoint.TransitionRepository(context.Background(), request.Action); err != nil {
		response.Error = err.Error()
	} else {
		response.Result = result
	}
	if err := s.encodeAndFlush(response); err != nil {
		return fmt.Errorf("unable to send repository transition response: %w", err)
	}

	// Success.
	return nil
}
