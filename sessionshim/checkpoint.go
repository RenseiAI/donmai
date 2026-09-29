package sessionshim

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

type continuationCall struct {
	request   shimwire.CheckpointRequest
	done      chan struct{}
	total     uint32
	digest    [32]byte
	bytes     []byte
	result    *attachwire.ContinuationCheckpoint
	err       error
	completed bool
}

// SupportsContinuation reports the explicitly selected complete-state rail.
func (c *Controller) SupportsContinuation() bool {
	return c != nil && c.selected >= shimwire.V5 && c.hello.Continuation != nil && c.hello.Continuation.Schema == attachwire.ContinuationSchema
}

// InspectContinuation returns a validated out-of-band complete checkpoint. The
// caller must already own and drain Events, discard frames at/before AtSeq, and
// require every subsequent raw canonical sequence. A Gap invalidates the
// handoff. Existing picture snapshots after AtSeq never reset the mirror.
// One request is in flight per controller; cancellation does not discard an
// unfinished response or allow a changed request to claim its chunks.
func (c *Controller) InspectContinuation(ctx context.Context, selected string) (attachwire.ContinuationCheckpoint, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return attachwire.ContinuationCheckpoint{}, err
	}
	if !c.SupportsContinuation() || selected != attachwire.ContinuationSchema {
		return attachwire.ContinuationCheckpoint{}, fmt.Errorf("sessionshim: %w: continuation not selected", shimwire.ErrVersionMismatch)
	}
	c.checkpointMu.Lock()
	if prior := c.checkpointCall; prior != nil && !prior.completed {
		c.checkpointMu.Unlock()
		return attachwire.ContinuationCheckpoint{}, fmt.Errorf("sessionshim: continuation request is already pending")
	}
	if c.nextCheckpointID == ^uint64(0) {
		c.checkpointMu.Unlock()
		return attachwire.ContinuationCheckpoint{}, fmt.Errorf("sessionshim: continuation request identity exhausted")
	}
	c.nextCheckpointID++
	call := &continuationCall{request: shimwire.CheckpointRequest{RequestID: c.nextCheckpointID, Generation: c.gen, Schema: selected}, done: make(chan struct{})}
	c.checkpointCall = call
	body, err := shimwire.EncodeCheckpointRequest(call.request)
	if err != nil {
		call.err = err
		call.completed = true
		close(call.done)
	}
	c.checkpointMu.Unlock()
	if err == nil {
		// One bounded sender per in-flight request keeps cancellation responsive
		// even when the shared connection writer is backpressured. Close tears
		// down the connection and unblocks the sender; no state lock spans I/O.
		go c.writeContinuationRequest(call, body)
	}
	select {
	case <-call.done:
	case <-ctx.Done():
		return attachwire.ContinuationCheckpoint{}, ctx.Err()
	case <-c.done:
		return attachwire.ContinuationCheckpoint{}, fmt.Errorf("sessionshim: continuation stream ended before result")
	}
	c.checkpointMu.Lock()
	defer c.checkpointMu.Unlock()
	if call.err != nil {
		return attachwire.ContinuationCheckpoint{}, call.err
	}
	if call.result == nil {
		return attachwire.ContinuationCheckpoint{}, fmt.Errorf("sessionshim: missing continuation result")
	}
	// Decode again to return owned slices; the retained retry result is immutable.
	return attachwire.DecodeContinuationCheckpoint(call.bytes, selected)
}

func (c *Controller) acceptContinuationChunk(body []byte) error {
	chunk, err := shimwire.DecodeCheckpointResult(body)
	if err != nil {
		return err
	}
	c.checkpointMu.Lock()
	defer c.checkpointMu.Unlock()
	call := c.checkpointCall
	if call == nil || call.completed || chunk.RequestID != call.request.RequestID || chunk.Generation != call.request.Generation {
		return fmt.Errorf("sessionshim: %w: continuation correlation", shimwire.ErrSnapshotMismatch)
	}
	if chunk.Code != "" {
		if len(call.bytes) != 0 {
			return fmt.Errorf("sessionshim: continuation refused after partial state")
		}
		call.err = fmt.Errorf("sessionshim: %w: %s", shimwire.ErrSnapshotRefused, chunk.Code)
		call.completed = true
		close(call.done)
		return nil
	}
	if uint64(chunk.Offset) != uint64(len(call.bytes)) {
		return fmt.Errorf("sessionshim: continuation chunk is not contiguous")
	}
	if len(call.bytes) == 0 {
		call.total = chunk.Total
		call.digest = chunk.Digest
	} else if chunk.Total != call.total || chunk.Digest != call.digest {
		return fmt.Errorf("sessionshim: continuation chunk changed identity")
	}
	call.bytes = append(call.bytes, chunk.Bytes...)
	if uint64(len(call.bytes)) != uint64(call.total) {
		return nil
	}
	if sha256.Sum256(call.bytes) != call.digest {
		return fmt.Errorf("sessionshim: continuation digest mismatch")
	}
	checkpoint, err := attachwire.DecodeContinuationCheckpoint(call.bytes, call.request.Schema)
	if err != nil {
		return err
	}
	if c.hello.Continuation == nil || checkpoint.Epoch != c.hello.Continuation.HostEpoch {
		return fmt.Errorf("sessionshim: continuation PTY epoch mismatch")
	}
	terminal, err := ptyhost.RestoreContinuation(checkpoint)
	if err != nil {
		return err
	}
	if err := terminal.Close(); err != nil {
		return err
	}
	call.result = &checkpoint
	call.completed = true
	close(call.done)
	return nil
}

func (s *Shim) dispatchContinuationRequest(ctrl *controllerConn, body []byte) error {
	if ctrl.selected < shimwire.V5 {
		return sendError(ctrl.w, shimwire.CodeMalformed, "continuation requires selected v5")
	}
	req, err := shimwire.DecodeCheckpointRequest(body)
	if err != nil {
		return sendError(ctrl.w, shimwire.CodeMalformed, "checkpoint request did not decode")
	}
	authorized := s.authorized(req.Generation)
	refuse := func(code shimwire.ErrorCode) error {
		b, err := shimwire.EncodeCheckpointResult(shimwire.CheckpointResult{RequestID: req.RequestID, Generation: req.Generation, Code: code})
		if err != nil {
			return err
		}
		if authorized && req.RequestID > ctrl.checkpointRequest.RequestID {
			ctrl.checkpointRequest = req
			ctrl.checkpointResult = []shimwire.Message{{Type: shimwire.TypeCheckpointResult, Body: b}}
		}
		return ctrl.w.WriteVersion(ctrl.selected, shimwire.TypeCheckpointResult, b)
	}
	if !authorized {
		return refuse(shimwire.CodeStaleGeneration)
	}
	if req.RequestID <= ctrl.checkpointRequest.RequestID {
		if req == ctrl.checkpointRequest {
			return ctrl.w.WriteVersionBatch(ctrl.selected, ctrl.checkpointResult...)
		}
		return refuse(shimwire.CodeDuplicateChanged)
	}
	checkpoint, err := s.sess.ContinuationCheckpoint(req.Schema)
	if err != nil {
		return refuse(shimwire.CodeInternal)
	}
	payload, err := checkpoint.Encode(req.Schema)
	if err != nil {
		return refuse(shimwire.CodeInternal)
	}
	if len(payload) > attachwire.MaxContinuationBytes {
		return refuse(shimwire.CodeInternal)
	}
	total := uint32(uint64(len(payload)) & 0xffffffff)
	digest := sha256.Sum256(payload)
	messages := make([]shimwire.Message, 0, (len(payload)+shimwire.CheckpointChunkBytes-1)/shimwire.CheckpointChunkBytes)
	for offset := 0; offset < len(payload); offset += shimwire.CheckpointChunkBytes {
		if offset < 0 || offset > attachwire.MaxContinuationBytes {
			return fmt.Errorf("sessionshim: checkpoint offset out of bounds")
		}
		end := min(offset+shimwire.CheckpointChunkBytes, len(payload))
		b, err := shimwire.EncodeCheckpointResult(shimwire.CheckpointResult{RequestID: req.RequestID, Generation: req.Generation, Total: total, Offset: uint32(offset), Digest: digest, Bytes: payload[offset:end]})
		if err != nil {
			return err
		}
		messages = append(messages, shimwire.Message{Type: shimwire.TypeCheckpointResult, Body: b})
	}
	ctrl.checkpointRequest = req
	ctrl.checkpointResult = messages
	return ctrl.w.WriteVersionBatch(ctrl.selected, messages...)
}

func (c *Controller) writeContinuationRequest(call *continuationCall, body []byte) {
	if err := c.w.WriteVersion(c.selected, shimwire.TypeCheckpointRequest, body); err != nil {
		c.checkpointMu.Lock()
		defer c.checkpointMu.Unlock()
		if !call.completed {
			call.err = err
			call.completed = true
			close(call.done)
		}
	}
}
