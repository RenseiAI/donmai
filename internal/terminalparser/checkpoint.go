package terminalparser

import (
	"fmt"

	"github.com/charmbracelet/x/ansi/parser"
)

const (
	parserCheckpointVersion = 1
	maxCheckpointDataSize   = 4 << 20
)

// ParserCheckpoint is a versioned snapshot of the byte-stream Parser state.
// It does not contain the parser's handlers; a restored parser keeps its own.
// Params contains the entire parameter buffer, including the in-progress slot.
// Data contains only live bytes. DataCapacity records the original data buffer
// length, which determines when a size-limited parser stops collecting bytes.
type ParserCheckpoint struct {
	Version      int
	Params       []int
	ParamsLen    int
	Data         []byte
	DataLen      int
	DataCapacity int
	Command      int
	State        byte
}

// ExportCheckpoint returns an independent snapshot of p's parser state.
func (p *Parser) ExportCheckpoint() (ParserCheckpoint, error) {
	if p == nil {
		return ParserCheckpoint{}, fmt.Errorf("export parser checkpoint: nil parser")
	}
	if p.dataLen < -1 || p.dataLen > len(p.data) {
		return ParserCheckpoint{}, fmt.Errorf("export parser checkpoint: invalid data length %d", p.dataLen)
	}

	data := p.data
	if p.dataLen >= 0 {
		data = data[:p.dataLen]
	}
	checkpoint := ParserCheckpoint{
		Version:      parserCheckpointVersion,
		Params:       p.params,
		ParamsLen:    p.paramsLen,
		Data:         data,
		DataLen:      p.dataLen,
		DataCapacity: len(p.data),
		Command:      p.cmd,
		State:        p.state,
	}
	if err := checkpoint.Validate(); err != nil {
		return ParserCheckpoint{}, fmt.Errorf("export parser checkpoint: %w", err)
	}
	checkpoint.Params = append([]int(nil), checkpoint.Params...)
	checkpoint.Data = append([]byte(nil), checkpoint.Data...)
	return checkpoint, nil
}

// RestoreCheckpoint replaces p's parser state after validating and copying
// the checkpoint. Invalid checkpoints leave p unchanged. Handlers are retained.
func (p *Parser) RestoreCheckpoint(checkpoint ParserCheckpoint) error {
	if p == nil {
		return fmt.Errorf("restore parser checkpoint: nil parser")
	}
	if err := checkpoint.Validate(); err != nil {
		return fmt.Errorf("restore parser checkpoint: %w", err)
	}

	params := append([]int(nil), checkpoint.Params...)
	var data []byte
	if checkpoint.DataLen < 0 {
		// StartAction distinguishes an unlimited empty buffer from nil.
		data = make([]byte, len(checkpoint.Data))
		copy(data, checkpoint.Data)
	} else {
		data = make([]byte, checkpoint.DataCapacity)
		copy(data, checkpoint.Data)
	}

	p.params = params
	p.paramsLen = checkpoint.ParamsLen
	p.data = data
	p.dataLen = checkpoint.DataLen
	p.cmd = checkpoint.Command
	p.state = checkpoint.State
	return nil
}

// Validate checks that a checkpoint is supported, bounded, and safe to import.
func (checkpoint ParserCheckpoint) Validate() error {
	if checkpoint.Version != parserCheckpointVersion {
		return fmt.Errorf("unsupported parser checkpoint version %d", checkpoint.Version)
	}
	if len(checkpoint.Params) == 0 || len(checkpoint.Params) > parser.MaxParamsSize {
		return fmt.Errorf("invalid parameter buffer size %d", len(checkpoint.Params))
	}
	if checkpoint.ParamsLen < 0 || checkpoint.ParamsLen > len(checkpoint.Params) {
		return fmt.Errorf("invalid parameter length %d", checkpoint.ParamsLen)
	}
	if checkpoint.DataCapacity < 0 || checkpoint.DataCapacity > maxCheckpointDataSize || len(checkpoint.Data) > maxCheckpointDataSize {
		return fmt.Errorf("invalid data buffer size %d", checkpoint.DataCapacity)
	}
	if checkpoint.DataLen == -1 {
		if checkpoint.DataCapacity != len(checkpoint.Data) {
			return fmt.Errorf("unlimited data capacity %d differs from live data length %d", checkpoint.DataCapacity, len(checkpoint.Data))
		}
	} else if checkpoint.DataLen < 0 || checkpoint.DataLen != len(checkpoint.Data) || checkpoint.DataLen > checkpoint.DataCapacity {
		return fmt.Errorf("invalid data length %d", checkpoint.DataLen)
	}
	if checkpoint.State > parser.Utf8State {
		return fmt.Errorf("invalid parser state %d", checkpoint.State)
	}
	if checkpoint.State == parser.Utf8State {
		if err := checkpoint.validateUtf8Prefix(); err != nil {
			return err
		}
	}
	return nil
}

func (checkpoint ParserCheckpoint) validateUtf8Prefix() error {
	if checkpoint.ParamsLen < 1 || checkpoint.ParamsLen >= 4 {
		return fmt.Errorf("invalid UTF-8 prefix length %d", checkpoint.ParamsLen)
	}
	first := byte(checkpoint.Command & 0xff)
	var width int
	switch {
	case first >= 0xc2 && first <= 0xdf:
		width = 2
	case first >= 0xe0 && first <= 0xef:
		width = 3
	case first >= 0xf0 && first <= 0xf4:
		width = 4
	default:
		return fmt.Errorf("invalid UTF-8 lead byte %#x", first)
	}
	if checkpoint.ParamsLen >= width {
		return fmt.Errorf("complete UTF-8 prefix in collection state")
	}
	// advanceUtf8 collects arbitrary subsequent bytes until width is reached.
	// Invalid continuation bytes are reachable and later print RuneError.
	return nil
}
