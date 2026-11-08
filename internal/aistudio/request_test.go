package aistudio

import (
	"encoding/json"
	"testing"
)

func TestLinkFunctionResults_OrphanResultGracefulFallback(t *testing.T) {
	// An orphan function result with empty Name and no pending calls
	content := Content{
		Role: RoleTool,
		Parts: []Part{
			{
				FunctionResult: &FunctionResult{
					ID:      "call_orphan_123",
					Content: json.RawMessage(`{"result":"some output from client"}`),
				},
			},
		},
	}

	var pendingCalls []FunctionCall
	linked, err := linkFunctionResults(content, &pendingCalls)
	if err != nil {
		t.Fatalf("expected orphan function result to degrade gracefully without error, got: %v", err)
	}

	if len(linked.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(linked.Parts))
	}

	if linked.Parts[0].FunctionResult != nil {
		t.Errorf("expected FunctionResult to be converted to Text part, but FunctionResult was still present")
	}

	if linked.Parts[0].Text != "some output from client" {
		t.Errorf("expected Text part to contain %q, got %q", "some output from client", linked.Parts[0].Text)
	}
}

func TestLinkFunctionResults_MatchByID(t *testing.T) {
	pendingCalls := []FunctionCall{
		{ID: "call_abc", Name: "exec_command"},
	}
	content := Content{
		Role: RoleTool,
		Parts: []Part{
			{
				FunctionResult: &FunctionResult{
					ID:      "call_abc",
					Content: json.RawMessage(`{"result":"done"}`),
				},
			},
		},
	}

	linked, err := linkFunctionResults(content, &pendingCalls)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if linked.Parts[0].FunctionResult == nil || linked.Parts[0].FunctionResult.Name != "exec_command" {
		t.Errorf("expected FunctionResult.Name to be 'exec_command', got: %#v", linked.Parts[0].FunctionResult)
	}

	if len(pendingCalls) != 0 {
		t.Errorf("expected pendingCalls to be consumed, remaining: %d", len(pendingCalls))
	}
}

func TestLinkFunctionResults_MatchByName(t *testing.T) {
	pendingCalls := []FunctionCall{
		{ID: "call_1", Name: "shell"},
		{ID: "call_2", Name: "local_shell"},
	}
	content := Content{
		Role: RoleTool,
		Parts: []Part{
			{
				FunctionResult: &FunctionResult{
					Name:    "local_shell",
					Content: json.RawMessage(`{"result":"ok"}`),
				},
			},
		},
	}

	linked, err := linkFunctionResults(content, &pendingCalls)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if linked.Parts[0].FunctionResult == nil || linked.Parts[0].FunctionResult.Name != "local_shell" {
		t.Errorf("expected FunctionResult.Name to be 'local_shell', got: %#v", linked.Parts[0].FunctionResult)
	}

	if len(pendingCalls) != 1 || pendingCalls[0].Name != "shell" {
		t.Errorf("expected remaining pendingCall to be 'shell', got: %#v", pendingCalls)
	}
}
