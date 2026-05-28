package tui

import (
	"context"
	"fmt"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/cluster"
)

// newNamespacesTable builds an empty namespaces picker table.
func newNamespacesTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "NAMESPACE", Width: 30},
			{Title: "PODS", Width: 6},
		}),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildNamespacesTable rebuilds rows from m.namespaces.
func (m *model) rebuildNamespacesTable() {
	rows := make([]table.Row, 0, len(m.namespaces))
	for _, ns := range m.namespaces {
		rows = append(rows, table.Row{ns.Name, fmt.Sprintf("%d", len(ns.Pods))})
	}
	m.namespacesTbl.SetRows(rows)
}

// loadAgentsCmd produces a tea.Cmd that calls Lister.ListAgents and
// emits an agentsLoadedMsg.
func loadAgentsCmd(lister cluster.Lister) tea.Cmd {
	return func() tea.Msg {
		ns, err := lister.ListAgents(context.Background())
		return agentsLoadedMsg{namespaces: ns, err: err}
	}
}

// newPickerModel constructs a model already in the Namespaces pane,
// wired with the given Lister and PortForwarder. Used when --endpoint
// is not given.
func newPickerModel(ctx context.Context, lister cluster.Lister, pf cluster.PortForwarder) *model {
	m := &model{
		lister:        lister,
		portForwarder: pf,
		pane:          paneNamespaces,
		namespacesTbl: newNamespacesTable(),
		podsTbl:       newPodsTable(),
	}
	return m
}
