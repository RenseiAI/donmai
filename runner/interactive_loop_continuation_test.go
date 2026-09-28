package runner

import (
	"context"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
)

type adapterContinuationFixture struct {
	agent.InteractiveSession
	calls  int
	ctx    context.Context
	schema string
}

func (*adapterContinuationFixture) SupportsContinuation() bool { return true }
func (s *adapterContinuationFixture) InspectContinuation(ctx context.Context, schema string) (attachwire.ContinuationCheckpoint, error) {
	s.calls++
	s.ctx = ctx
	s.schema = schema
	return attachwire.ContinuationCheckpoint{Schema: schema}, nil
}

func TestSessAdapterContinuationForwarding(t *testing.T) {
	underlying := &adapterContinuationFixture{}
	adapter := sessAdapter{underlying}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !adapter.SupportsContinuation() {
		t.Fatal("adapter hid real continuation support")
	}
	checkpoint, err := adapter.InspectContinuation(ctx, attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	if underlying.calls != 1 || underlying.ctx != ctx || underlying.schema != attachwire.ContinuationSchema || checkpoint.Schema != attachwire.ContinuationSchema {
		t.Fatal("adapter discarded context/schema or source result")
	}
	legacy := sessAdapter{}
	if legacy.SupportsContinuation() {
		t.Fatal("legacy adapter advertised support")
	}
	if _, err := legacy.InspectContinuation(ctx, attachwire.ContinuationSchema); err == nil {
		t.Fatal("legacy adapter accepted continuation")
	}
}
