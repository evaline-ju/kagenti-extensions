package tui

import (
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/cluster"
)

// fakeLister returns a fixed []AgentNamespace.
type fakeLister struct{ namespaces []cluster.AgentNamespace }

func (f *fakeLister) ListAgents(ctx context.Context) ([]cluster.AgentNamespace, error) {
	return f.namespaces, nil
}

// fixtureNamespaces is a small, deterministic dataset for picker tests.
var fixtureNamespaces = []cluster.AgentNamespace{
	{Name: "team1", Pods: []cluster.Pod{
		{Namespace: "team1", Name: "weather-agent-1", Phase: "Running", Ready: true},
	}},
	{Name: "team2", Pods: []cluster.Pod{
		{Namespace: "team2", Name: "billing-agent-1", Phase: "Pending", Ready: false},
	}},
}

func TestNamespacesPaneLoadsAndRenders(t *testing.T) {
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	// Init returns a Cmd that loads the agents.
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned nil cmd; want loader cmd")
	}
	msg := cmd()
	loaded, ok := msg.(agentsLoadedMsg)
	if !ok {
		t.Fatalf("loader cmd produced %T, want agentsLoadedMsg", msg)
	}
	updated, _ := m.Update(loaded)
	mm := updated.(*model)
	if len(mm.namespaces) != 2 {
		t.Fatalf("model should hold 2 namespaces, got %d", len(mm.namespaces))
	}
	view := mm.View()
	if !contains(view, "team1") || !contains(view, "team2") {
		t.Fatalf("rendered view missing namespaces:\n%s", view)
	}
}

func TestNamespacesPaneDrillsIntoPods(t *testing.T) {
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	loaded := m.Init()()
	updated, _ := m.Update(loaded)
	mm := updated.(*model)
	// Press Enter on the first row.
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = updated.(*model)
	if mm.pane != panePods {
		t.Fatalf("after Enter, active pane should be panePods, got %v", mm.pane)
	}
	if mm.selectedNamespace != "team1" {
		t.Fatalf("selected namespace should be team1, got %q", mm.selectedNamespace)
	}
}

// contains is a thin wrapper over strings.Contains used to keep test
// assertions readable.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// silence unused-import nag if test build trims this file later
var _ = time.Second
