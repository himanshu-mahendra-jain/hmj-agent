package session

import (
	"bufio"
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/logging"
)

// ErrHistoryNotSaved is returned, wrapped, by ApplyCompaction when the compaction was applied but
// the messages it replaced could not be kept for /history.
var ErrHistoryNotSaved = errors.New("the earlier turns were not kept for /history")

// The messages a compaction replaces are kept in the conversation's history file, one JSON message
// per line, for /history only; they are never sent again. Only the first ArchivedBytes of the file
// count: a write that the conversation file never recorded, because hmj stopped in between, is ignored
// and overwritten by the next one.

// historyRoot returns the directory that holds the conversations' history files, in the
// session directory, or "" when the conversations are not saved.
func (sm *SessionManager) historyRoot() string {
	if sm.dir == "" {
		return ""
	}
	return filepath.Join(sm.dir, "history")
}

// historyPath returns the history file of the conversation with the given ID, or "".
func (sm *SessionManager) historyPath(id string) string {
	if root := sm.historyRoot(); root != "" {
		return filepath.Join(root, id+".jsonl")
	}
	return ""
}

// pruneHistory removes the history files of conversations without a conversation file once
// orphanScratchMaxIdle has passed, since another hmj may have just created the conversation.
func (sm *SessionManager) pruneHistory() {
	root := sm.historyRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || e.IsDir() {
			continue
		}
		info, err := e.Info()
		if _, exists := sm.Conversations[id]; !exists && err == nil && now.Sub(info.ModTime()) > orphanScratchMaxIdle {
			if err := os.Remove(filepath.Join(root, e.Name())); err != nil {
				logging.L().Warn("orphaned history not removed", "file", e.Name(), "error", err.Error())
			}
		}
	}
}

// archive keeps msgs for /history, leaving out compactions. Only what a turn shows is kept: the
// text, the tool calls and the tool results, not reasoning blocks or pictures. An unsaved
// conversation keeps them in memory.
func (c *Conversation) archive(msgs []chat.Message) error {
	var kept []chat.Message
	for i := range msgs {
		m := &msgs[i]
		if m.CompactionBlock() != nil {
			continue
		}
		trimmed := chat.Message{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			trimmed.ToolCalls = append(trimmed.ToolCalls, chat.ToolCall{ID: tc.ID, Type: tc.Type, Function: tc.Function})
		}
		kept = append(kept, trimmed)
	}
	if len(kept) == 0 {
		return nil
	}
	if c.HistoryPath == "" {
		c.archived = append(c.archived, kept...)
		return nil
	}

	var buf bytes.Buffer
	for _, m := range kept {
		if err := json.MarshalWrite(&buf, m); err != nil {
			return err
		}
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(c.HistoryPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(c.HistoryPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	// A file shorter than the mark lost its end; the new messages follow what is left.
	offset := c.ArchivedBytes
	if info, err := f.Stat(); err == nil && info.Size() < offset {
		offset = info.Size()
	}
	if err := f.Truncate(offset); err != nil {
		return errors.Join(err, f.Close())
	}
	if _, err := f.WriteAt(buf.Bytes(), offset); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	c.ArchivedBytes = offset + int64(buf.Len())
	return nil
}

// EarlierMessages returns the messages that compactions replaced, oldest first, as archive kept
// them. A conversation compacted before history files existed, or never compacted, has none. When
// the file cannot be read in full, the messages read before the fault are returned with the error.
func (c *Conversation) EarlierMessages() ([]chat.Message, error) {
	if c.HistoryPath == "" {
		return append([]chat.Message(nil), c.archived...), nil
	}
	if c.ArchivedBytes == 0 {
		return nil, nil
	}
	f, err := os.Open(c.HistoryPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var msgs []chat.Message
	r := bufio.NewReader(io.LimitReader(f, c.ArchivedBytes))
	for line := 1; ; line++ {
		data, err := r.ReadBytes('\n')
		if len(data) > 0 {
			var m chat.Message
			if jerr := json.Unmarshal(data, &m); jerr != nil {
				return msgs, fmt.Errorf("%s line %d: %w", c.HistoryPath, line, jerr)
			}
			msgs = append(msgs, m)
		}
		if err == io.EOF {
			return msgs, nil
		}
		if err != nil {
			return msgs, err
		}
	}
}
