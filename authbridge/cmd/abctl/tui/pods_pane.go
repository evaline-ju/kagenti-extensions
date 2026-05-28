package tui

import (
	"github.com/charmbracelet/bubbles/table"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/cluster"
)

// newPodsTable is the stub for the Pods picker table; populated in Task 6.
func newPodsTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{{Title: "POD", Width: 40}}),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildPodsTable populates pod rows for m.selectedNamespace; fleshed
// out in Task 6.
func (m *model) rebuildPodsTable() {
	// Stub — Task 6 fills this in.
}

// currentPodsList returns the slice of pods backing the Pods pane,
// keyed by the currently-selected namespace. Stub — Task 6 fills this in.
func (m *model) currentPodsList() []cluster.Pod {
	return nil
}
