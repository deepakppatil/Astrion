package mock

import (
	"testing"

	"github.com/deepakppatil/astrion/internal/agent"
	"github.com/deepakppatil/astrion/internal/domain"
)

func TestDecoderNormalizesEvents(t *testing.T) {
	d := New().NewDecoder()
	cases := []struct {
		line string
		want agent.EventType
	}{
		{`plain text output`, agent.EventLog},
		{`{"type":"progress","step":"analyze","message":"reading"}`, agent.EventProgress},
		{`{"type":"interaction","id":"q-1","kind":"MULTIPLE_CHOICE","prompt":"Pick","options":[{"value":"a","label":"A"}]}`, agent.EventInteraction},
		{`{"type":"artifact","path":"spec.md","artifactType":"specification"}`, agent.EventArtifact},
		{`{"type":"result","status":"success","summary":"done"}`, agent.EventResult},
		{`{"type":"unknown-thing"}`, agent.EventLog},
		{`{not json`, agent.EventLog},
	}
	for _, c := range cases {
		evs := d.Decode([]byte(c.line))
		if len(evs) != 1 || evs[0].Type != c.want {
			t.Fatalf("%s: got %+v, want %s", c.line, evs, c.want)
		}
	}
	if evs := d.Decode([]byte("   ")); len(evs) != 0 {
		t.Fatal("blank lines must be ignored")
	}
	ev := d.Decode([]byte(`{"type":"interaction","id":"q-1","kind":"MULTIPLE_CHOICE","prompt":"Pick","options":[{"value":"a","label":"A"}]}`))[0]
	if ev.Interaction.AgentRequestID != "q-1" || ev.Interaction.Kind != domain.InteractionMultipleChoice || len(ev.Interaction.Options) != 1 {
		t.Fatalf("interaction = %+v", ev.Interaction)
	}
}

func TestDisposition(t *testing.T) {
	a := New()
	ok := &agent.Result{Status: "success"}
	if a.Disposition(0, ok, false) != agent.DispositionCompleted {
		t.Fatal("success")
	}
	if a.Disposition(0, nil, false) != agent.DispositionFailed {
		t.Fatal("exit 0 without a success result must not count as success")
	}
	if a.Disposition(ExitAwaitingInput, nil, true) != agent.DispositionSuspended {
		t.Fatal("awaiting input")
	}
	if a.Disposition(ExitAwaitingInput, nil, false) != agent.DispositionFailed {
		t.Fatal("exit 75 without a recorded question is a failure")
	}
}

func TestInvocationNeverPutsInputsInArgv(t *testing.T) {
	inv, err := New().Invocation(agent.InvocationRequest{Command: "/write-spec", Inputs: map[string]any{"specName": "secret-ish value"},
		WorkDir: "/w", OutputDir: "/o", CheckpointDir: "/s/cp", Mode: domain.ModeCheckpointResume})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range inv.Args {
		if a == "secret-ish value" {
			t.Fatal("inputs must be passed via files, not argv")
		}
	}
	if len(inv.Files) == 0 {
		t.Fatal("expected invocation file")
	}
}
