package main

import (
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

var (
	styleTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	styleCursor   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	styleSelected = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styleHint     = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Italic(true)
)

// interactive reports whether we can run a TUI (stdin+stdout are a terminal).
func interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// ---- select prompt ---------------------------------------------------------

type selectItem struct {
	label string
	desc  string // optional dim suffix
}

type selectModel struct {
	title  string
	items  []selectItem
	cursor int
	chosen int
	done   bool
}

func (m selectModel) Init() tea.Cmd { return nil }

func (m selectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.items)-1 {
				m.cursor++
			}
		case "enter", " ":
			m.chosen = m.cursor
			m.done = true
			return m, tea.Quit
		case "q", "esc", "ctrl+c":
			m.chosen = -1
			m.done = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m selectModel) View() string {
	if m.done {
		return ""
	}
	var b strings.Builder
	b.WriteString(styleTitle.Render(m.title) + "\n\n")
	for i, it := range m.items {
		cursor := "  "
		label := it.label
		if i == m.cursor {
			cursor = styleCursor.Render("❯ ")
			label = styleSelected.Render(label)
		}
		b.WriteString(cursor + label)
		if it.desc != "" {
			b.WriteString("  " + styleDim.Render(it.desc))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n" + styleHint.Render("↑/↓ move · enter select · esc back"))
	return b.String()
}

// runSelect shows a menu and returns the chosen index, or ok=false if cancelled.
func runSelect(title string, items []selectItem) (int, bool) {
	m, err := tea.NewProgram(selectModel{title: title, items: items, chosen: -1}).Run()
	if err != nil {
		return -1, false
	}
	sm := m.(selectModel)
	return sm.chosen, sm.chosen >= 0
}

// runSelectStrings is a convenience for label-only menus.
func runSelectStrings(title string, labels ...string) (int, bool) {
	items := make([]selectItem, len(labels))
	for i, l := range labels {
		items[i] = selectItem{label: l}
	}
	return runSelect(title, items)
}

// ---- text input prompt -----------------------------------------------------

type inputModel struct {
	label string
	def   string
	value string
	done  bool
	ok    bool
}

func (m inputModel) Init() tea.Cmd { return nil }

func (m inputModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.Type {
		case tea.KeyEnter:
			m.done, m.ok = true, true
			return m, tea.Quit
		case tea.KeyEsc, tea.KeyCtrlC:
			m.done = true
			return m, tea.Quit
		case tea.KeyBackspace:
			if len(m.value) > 0 {
				m.value = m.value[:len(m.value)-1]
			}
		case tea.KeyRunes, tea.KeySpace:
			m.value += string(key.Runes)
		}
	}
	return m, nil
}

func (m inputModel) View() string {
	if m.done {
		return ""
	}
	prompt := styleTitle.Render(m.label)
	if m.def != "" {
		prompt += styleDim.Render(" ["+m.def+"]")
	}
	return prompt + ": " + m.value + "▏\n" + styleHint.Render("enter confirm · esc cancel")
}

// runInput prompts for a line of text; empty input returns def. ok=false if
// cancelled.
func runInput(label, def string) (string, bool) {
	m, err := tea.NewProgram(inputModel{label: label, def: def}).Run()
	if err != nil {
		return "", false
	}
	im := m.(inputModel)
	if !im.ok {
		return "", false
	}
	v := strings.TrimSpace(im.value)
	if v == "" {
		return def, true
	}
	return v, true
}

// confirm asks a yes/no question (default No).
func confirm(question string) bool {
	i, ok := runSelectStrings(question, "No", "Yes")
	return ok && i == 1
}

// notify prints a short styled line between menus.
func notify(format string, a ...any) {
	fmt.Println(styleSelected.Render(fmt.Sprintf(format, a...)))
}
