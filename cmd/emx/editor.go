package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// editInEditor opens initial in the user's $EDITOR (falling back to a sensible
// default) and returns the edited text. changed is false when the content is
// unchanged, letting callers skip a no-op save. The temp file carries the given
// extension (e.g. ".json") so editors enable syntax highlighting.
func editInEditor(initial, ext string) (edited string, changed bool, err error) {
	f, err := os.CreateTemp("", "emx-edit-*"+ext)
	if err != nil {
		return "", false, err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(initial); err != nil {
		f.Close()
		return "", false, err
	}
	f.Close()

	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = defaultEditor()
	}
	// $EDITOR may include args (e.g. "code --wait"); split on spaces.
	parts := strings.Fields(editor)
	args := append(parts[1:], path)
	cmd := exec.Command(parts[0], args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("editor %q failed: %w", editor, err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	return string(out), string(out) != initial, nil
}

// defaultEditor picks an editor that exists on PATH, preferring nano for
// friendliness, then vim/vi.
func defaultEditor() string {
	for _, e := range []string{"nano", "vim", "vi"} {
		if p, err := exec.LookPath(e); err == nil {
			return filepath.Base(p)
		}
	}
	return "vi"
}
