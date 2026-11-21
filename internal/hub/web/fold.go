package web

import "strings"

// foldPreviewLines is how many lines of a folded command stay visible.
const foldPreviewLines = 3

// FoldBlock is a copyable block of text for the "fold" template. A block of
// more than foldPreviewLines lines is folded: a keyboard-operable <details>
// whose summary shows the first lines, with a Copy button that stays visible
// and copies the whole text. A short block is a plain copy field.
type FoldBlock struct {
	// ID is the id of the element that holds the full text; the Copy button
	// copies from it.
	ID string
	// Label is the accessible name of the Copy button.
	Label string
	// Text is the full text. Preview is its first lines. Lines counts them.
	Text, Preview string
	Lines         int
	Folded        bool
}

// foldBlock builds the block for text. The text is never changed or cut: only
// the preview is.
func foldBlock(id, label, text string) FoldBlock {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	b := FoldBlock{ID: id, Label: label, Text: text, Lines: len(lines)}
	if len(lines) > foldPreviewLines {
		b.Folded = true
		b.Preview = strings.Join(lines[:foldPreviewLines], "\n")
	}
	return b
}
