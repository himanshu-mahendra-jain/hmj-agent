package terminal

import "strings"

// MarkdownStreamer renders the model's streamed Markdown a line at a time, as each line is
// complete: tables are held until their last row and drawn, other lines are rendered as they
// come (see renderLine), and code fences are passed through as written.
type MarkdownStreamer struct {
	out        func(string)
	lineBuf    strings.Builder
	tableLines []string
	inTable    bool
	block      markdownBlock
}

// NewMarkdownStreamer creates a MarkdownStreamer sending rendered text to out.
func NewMarkdownStreamer(out func(string)) *MarkdownStreamer {
	return &MarkdownStreamer{out: out, block: markdownBlock{pad: padMarkdown()}}
}

func isPotentialTableRow(line string) bool {
	trimmed := strings.TrimSpace(line)
	return len(trimmed) >= 2 && strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|")
}

func (s *MarkdownStreamer) processLine(line string) {
	if !s.block.inFence() && isPotentialTableRow(line) {
		s.tableLines = append(s.tableLines, line)
		s.inTable = true
		return
	}

	if s.inTable {
		s.flushTable()
	}
	s.out(s.block.show(line) + "\n")
}

func (s *MarkdownStreamer) flushTable() {
	if len(s.tableLines) == 0 {
		s.inTable = false
		return
	}
	if tbl, ok := ParseMarkdownTable(s.tableLines); ok {
		if s.block.pad {
			s.out(padBlock(tbl.render(renderTableWidth() - len(markdownPad))))
		} else {
			s.out(tbl.Render())
		}
	} else {
		for _, l := range s.tableLines {
			s.out(s.block.show(l) + "\n")
		}
	}
	s.tableLines = nil
	s.inTable = false
}

// Write processes a streamed chunk of text from the assistant.
func (s *MarkdownStreamer) Write(chunk string) {
	s.lineBuf.WriteString(chunk)
	content := s.lineBuf.String()
	for {
		idx := strings.IndexByte(content, '\n')
		if idx == -1 {
			break
		}
		line := content[:idx]
		content = content[idx+1:]
		s.processLine(line)
	}
	s.lineBuf.Reset()
	s.lineBuf.WriteString(content)
}

// Flush flushes any pending buffered lines at the end of generation.
func (s *MarkdownStreamer) Flush() {
	if s.lineBuf.Len() > 0 {
		line := s.lineBuf.String()
		s.lineBuf.Reset()
		if !s.block.inFence() && isPotentialTableRow(line) {
			// Open the table as processLine would, or the row would never be written.
			s.tableLines = append(s.tableLines, line)
			s.inTable = true
		} else {
			if s.inTable {
				s.flushTable()
			}
			s.out(s.block.show(line))
		}
	}
	if s.inTable {
		s.flushTable()
	}
}
