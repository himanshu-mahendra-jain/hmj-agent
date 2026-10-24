package terminal

// SetWidth fixes the editor's width instead of the terminal's.
func (e *PromptEditor) SetWidth(w int) { e.width = w }

// Text is the editor's content.
func (e *PromptEditor) Text() string { return string(e.runes) }

// Cursor is the cursor's offset in runes.
func (e *PromptEditor) Cursor() int { return e.cursor }
