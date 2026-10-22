package session

import (
	"bufio"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/redact"
	"hmj-agent/internal/terminal/theme"
)

const (
	DefaultSystemPrompt = "You are HMJ Agent, an expert AI software engineer and terminal coding assistant. Always be truthful, transparent, and direct about your identity, tools, and technical capabilities; never adopt fictional personas or mimic third-party tools. You have access to tools to inspect files, edit files, create files, list directories, and run shell commands. You run on a dedicated machine with unrestricted access: you may read and write any path and run any command. Treat the current working directory as the project root unless told otherwise. Be concise, precise, and direct. When reading files, read the file to understand its full context before editing. When creating a file, give it a clear, descriptive name that states its purpose, so any reader, human or LLM agent, can tell what the file does from its name alone without opening it (for example, retry_backoff_test.go or deploy_staging.sh rather than utils.go, helper2.py or temp.sh); follow the project's existing naming conventions and casing. Project guidelines (README.md or AGENTS.md) must never exceed 160 lines; when editing them, prune obsolete content to stay strictly within 160 lines. Before declaring any coding or refactoring task complete, verify your work: run the relevant build, test, or linter commands via run_command to ensure no syntax errors or regressions were introduced. If tests or checks fail, diagnose and fix the errors before concluding. Once verified, stop calling tools and summarize your result directly."

	// MaxGuidelineLines caps how many lines of workspace guidelines (README.md / AGENTS.md)
	// are loaded into the system prompt to keep context concise and cache-friendly.
	MaxGuidelineLines = 160
)

// guidelineCacheEntry memoizes one successfully loaded guideline file.
type guidelineCacheEntry struct {
	modTime time.Time
	size    int64
	prompt  string
}

var (
	guidelineMu    sync.Mutex
	guidelineCache = make(map[string]guidelineCacheEntry)
)

// CurrentSystemPrompt returns DefaultSystemPrompt augmented with README.md from the current working directory if present.
func CurrentSystemPrompt() string {
	wd, err := os.Getwd()
	if err != nil {
		return DefaultSystemPrompt
	}
	return LoadSystemPrompt(wd)
}

// LoadSystemPrompt returns DefaultSystemPrompt combined with guidelines from AGENTS.md,
// agents.md, or README.md in workspaceDir if found (streamed and capped at MaxGuidelineLines).
// Successful loads are cached per file (keyed on mtime+size) so per-turn prompt
// refreshes cost a stat instead of a full read.
func LoadSystemPrompt(workspaceDir string) string {
	candidates := []string{
		"AGENTS.md",
		"agents.md",
		"README.md",
		"readme.md",
		"README",
		"Readme.md",
	}
	for _, name := range candidates {
		p := filepath.Join(workspaceDir, name)

		fi, statErr := os.Stat(p)
		if statErr != nil {
			continue // candidate absent or unstatable; try the next name
		}
		guidelineMu.Lock()
		cached, hasCache := guidelineCache[p]
		guidelineMu.Unlock()
		if hasCache && cached.modTime.Equal(fi.ModTime()) && cached.size == fi.Size() {
			return cached.prompt
		}

		f, err := os.Open(p)
		if err != nil {
			continue // candidate unreadable; try the next name
		}
		scanner := bufio.NewScanner(f)
		// Allow long lines (up to 1MB) so minified files are not silently cut
		// at bufio's 64KB default mid-file.
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		var lines []string
		for scanner.Scan() && len(lines) < MaxGuidelineLines {
			lines = append(lines, scanner.Text())
		}
		scanErr := scanner.Err()
		closeErr := f.Close()
		if scanErr != nil || closeErr != nil {
			// Read failed mid-file (I/O error or a line beyond the 1MB cap):
			// never feed a silently truncated guideline into the prompt — skip
			// this candidate entirely and fall through to the next one.
			continue
		}

		content := strings.TrimSpace(strings.Join(lines, "\n"))
		if content == "" {
			continue // empty/whitespace-only candidate; try the next name
		}
		prompt := fmt.Sprintf("%s\n\n---\n# Project Context & Guidelines (%s)\n\n%s",
			DefaultSystemPrompt, name, content)
		guidelineMu.Lock()
		guidelineCache[p] = guidelineCacheEntry{modTime: fi.ModTime(), size: fi.Size(), prompt: prompt}
		guidelineMu.Unlock()
		return prompt
	}
	return DefaultSystemPrompt
}

// Conversation is one named chat session: its model and the transcript of messages exchanged in it.
type Conversation struct {
	// ID is the unique identifier of the conversation within its SessionManager.
	ID string `json:"id"`
	// Name is the title shown in listings. It may be set automatically from the first prompt.
	Name string `json:"name"`
	// AutoTitled marks whether Name was set automatically rather than chosen by the user. Only an
	// automatic name may be replaced by a generated one; a rename clears it.
	AutoTitled bool `json:"autoTitled,omitempty"`
	// Model is the provider and model this conversation talks to.
	Model config.ModelRef `json:"model"`
	// Messages is the transcript, led by the system prompt once SyncSystemPrompt has run.
	Messages []chat.Message `json:"messages"`
	// CreatedAt records when the conversation was created.
	CreatedAt time.Time `json:"createdAt"`
	// UpdatedAt records when the conversation was last changed.
	UpdatedAt time.Time `json:"updatedAt"`
	// Pruned marks whether this conversation's older tool outputs have
	// already been compacted, so prune passes can skip them cheaply.
	Pruned bool `json:"pruned,omitempty"`
	// LastPromptTokens is the prompt size the provider reported for the latest request. It is the
	// baseline for EstimateTokens, which adds only the bytes appended since TokenMarkIndex.
	LastPromptTokens int `json:"lastPromptTokens,omitzero"`
	// TokenMarkIndex is the index of the first message not included in LastPromptTokens.
	TokenMarkIndex int `json:"tokenMarkIndex,omitzero"`
	// ThinkingKeptValid marks a conversation whose pruning and condensing have always stripped
	// the thinking blocks after the messages they edit. A conversation saved without it by an
	// earlier version may carry stale blocks after such edits; they are stripped once on load.
	ThinkingKeptValid bool `json:"thinkingKeptValid,omitempty"`
}

// UserMessageCount returns the number of user messages in the transcript.
func (c *Conversation) UserMessageCount() int {
	count := 0
	for _, m := range c.Messages {
		if m.Role == "user" {
			count++
		}
	}
	return count
}

// SetPromptTokens records the prompt size the provider reported for a request sent with the
// current messages. Messages appended afterwards are estimated on top of it.
func (c *Conversation) SetPromptTokens(tokens int) {
	c.LastPromptTokens = tokens
	c.TokenMarkIndex = len(c.Messages)
}

// CancelledToolResult is the output recorded for a tool call that was halted before it ran.
const CancelledToolResult = "[cancelled by user before this tool ran]"

// resetTokenMark forgets the provider's prompt size. It must be called whenever the message slice
// or its content is rewritten, since the reported size no longer matches the transcript.
func (c *Conversation) resetTokenMark() {
	c.LastPromptTokens = 0
	c.TokenMarkIndex = 0
}

// SyncSystemPrompt sets prompt as the leading system message, inserting one when the transcript
// does not start with a system message.
func (c *Conversation) SyncSystemPrompt(prompt string) {
	if len(c.Messages) == 0 {
		c.Messages = append(c.Messages, chat.Message{
			Role:    "system",
			Content: prompt,
		})
		return
	}
	if c.Messages[0].Role == "system" {
		c.Messages[0].Content = prompt
	} else {
		c.Messages = append([]chat.Message{{Role: "system", Content: prompt}}, c.Messages...)
	}
}

// SessionManager holds the conversations, the active one and the default model, and saves them to a
// session file.
type SessionManager struct {
	ActiveID      string                   `json:"activeId"`
	Conversations map[string]*Conversation `json:"conversations"`
	Order         []string                 `json:"order"`
	DefaultModel  config.ModelRef          `json:"defaultModel"`
	counter       int
	storagePath   string
	mu            sync.RWMutex
}

// NewSessionManager returns an in-memory manager with one empty conversation named default and
// defaultModel as its default model.
func NewSessionManager(defaultModel config.ModelRef) *SessionManager {
	sm := &SessionManager{
		Conversations: make(map[string]*Conversation),
		DefaultModel:  defaultModel,
		counter:       0,
	}
	sm.New("default")
	return sm
}

// savedSessionFile is the on-disk layout of the session storage file.
type savedSessionFile struct {
	ActiveID      string                   `json:"activeId"`
	Conversations map[string]*Conversation `json:"conversations"`
	Order         []string                 `json:"order"`
	Counter       int                      `json:"counter"`
	DefaultModel  config.ModelRef          `json:"defaultModel"`
}

// readSavedSessions reads and decodes the session file at storagePath. It reports false
// when there is no usable file. A corrupt file is moved aside and reported as a warning.
func readSavedSessions(storagePath string) (savedSessionFile, bool) {
	var saved savedSessionFile

	data, err := os.ReadFile(storagePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "warning: could not read session file %s: %v\n", storagePath, err)
		}
		return saved, false
	}

	if err := json.Unmarshal(data, &saved); err != nil {
		corruptPath := fmt.Sprintf("%s.corrupt.%s", storagePath, time.Now().Format("20060102150405"))
		fmt.Fprintln(os.Stderr, theme.Caution("Session history could not be read and has been reset."))
		if renameErr := os.Rename(storagePath, corruptPath); renameErr == nil {
			fmt.Fprintf(os.Stderr, "  The damaged file was kept as %s\n", corruptPath)
			pruneCorruptBackups(storagePath)
		} else {
			fmt.Fprintf(os.Stderr, "  The damaged file could not be moved aside: %v\n", renameErr)
		}
		fmt.Fprintf(os.Stderr, "  Reason: %v\n", err)
		return saved, false
	}
	return saved, true
}

// maxCorruptBackups is how many damaged session files are kept for recovery.
const maxCorruptBackups = 5

// pruneCorruptBackups deletes all but the newest maxCorruptBackups damaged copies of
// storagePath. Their timestamp suffixes sort chronologically by name.
func pruneCorruptBackups(storagePath string) {
	backups, err := filepath.Glob(storagePath + ".corrupt.*")
	if err != nil || len(backups) <= maxCorruptBackups {
		return
	}
	slices.Sort(backups)
	for _, old := range backups[:len(backups)-maxCorruptBackups] {
		_ = os.Remove(old)
	}
}

// restoreOrder drops order entries whose conversations no longer exist and appends any
// conversation that is missing from the order.
func (sm *SessionManager) restoreOrder() {
	validOrder := make([]string, 0, len(sm.Order))
	for _, id := range sm.Order {
		if _, ok := sm.Conversations[id]; ok {
			validOrder = append(validOrder, id)
		}
	}
	sm.Order = validOrder

	for id := range sm.Conversations {
		found := slices.Contains(sm.Order, id)
		if !found {
			sm.Order = append(sm.Order, id)
		}
	}
}

// ensureActiveConversation keeps ActiveID pointing at a loaded conversation, falling back
// to the last ordered one. It reports false when no conversation is left to activate.
func (sm *SessionManager) ensureActiveConversation() bool {
	if _, ok := sm.Conversations[sm.ActiveID]; ok {
		return true
	}
	if len(sm.Order) == 0 {
		return false
	}
	sm.ActiveID = sm.Order[len(sm.Order)-1]
	return true
}

// finishLoadedConversations recovers the ID counter, fills in missing models, repairs tool calls
// left unanswered by an interrupted turn, and refreshes the system prompt of conversations that have
// not started yet.
func (sm *SessionManager) finishLoadedConversations(defaultModel config.ModelRef) {
	for id, conv := range sm.Conversations {
		// The map key is the ID that the order, the active ID and every lookup use.
		conv.ID = id
		// Non-numeric IDs (e.g. "default") don't participate in counter
		// recovery; that is expected, not a swallowed error.
		if idNum, err := strconv.Atoi(conv.ID); err == nil && idNum > sm.counter {
			sm.counter = idNum
		}
		if conv.Model.Model == "" {
			conv.Model = defaultModel
		}
		if repaired := repairToolCalls(conv.Messages); len(repaired) != len(conv.Messages) {
			conv.Messages = repaired
			conv.resetTokenMark()
		}
		if conv.UserMessageCount() == 0 {
			conv.SyncSystemPrompt(CurrentSystemPrompt())
		}
		if !conv.ThinkingKeptValid {
			if conv.Pruned {
				conv.stripThinkingFrom(0)
			}
			conv.ThinkingKeptValid = true
		}
	}
}

// repairToolCalls returns msgs with a placeholder tool result appended for every tool call that
// has none. Transcripts saved before cancelled turns were repaired can contain such calls, which
// providers reject. msgs is returned unchanged when nothing is missing.
func repairToolCalls(msgs []chat.Message) []chat.Message {
	var out []chat.Message
	for i := 0; i < len(msgs); i++ {
		out = append(out, msgs[i])
		if msgs[i].Role != "assistant" || len(msgs[i].ToolCalls) == 0 {
			continue
		}
		answered := make(map[string]bool)
		j := i + 1
		for ; j < len(msgs) && msgs[j].Role == "tool"; j++ {
			answered[msgs[j].ToolCallID] = true
			out = append(out, msgs[j])
		}
		for _, tc := range msgs[i].ToolCalls {
			if !answered[tc.ID] {
				out = append(out, chat.Message{Role: "tool", ToolCallID: tc.ID, Content: CancelledToolResult})
				answered[tc.ID] = true
			}
		}
		i = j - 1
	}
	if len(out) == len(msgs) {
		return msgs
	}
	return out
}

// LoadSessionManager loads the session file at storagePath, falling back to a fresh default
// conversation when there is no usable file. Conversations without a model get defaultModel, and
// each system prompt is refreshed.
func LoadSessionManager(storagePath string, defaultModel config.ModelRef) *SessionManager {
	sm := &SessionManager{
		Conversations: make(map[string]*Conversation),
		DefaultModel:  defaultModel,
		storagePath:   storagePath,
	}

	if storagePath == "" {
		sm.New("default")
		return sm
	}

	saved, ok := readSavedSessions(storagePath)
	// A hand-edited or partly written file can hold null conversations; they are dropped
	// instead of crashing every start.
	maps.DeleteFunc(saved.Conversations, func(_ string, c *Conversation) bool { return c == nil })
	if !ok || len(saved.Conversations) == 0 {
		sm.New("default")
		return sm
	}

	sm.Conversations = saved.Conversations
	sm.Order = saved.Order
	sm.counter = saved.Counter
	sm.ActiveID = saved.ActiveID
	if saved.DefaultModel.Model != "" {
		sm.DefaultModel = saved.DefaultModel
	} else if cur, ok := sm.Conversations[sm.ActiveID]; ok && cur.Model.Model != "" {
		sm.DefaultModel = cur.Model
	}

	sm.restoreOrder()
	if !sm.ensureActiveConversation() {
		sm.New("default")
		return sm
	}

	sm.finishLoadedConversations(defaultModel)
	return sm
}

// SetStoragePath sets the file that Save writes to. An empty path disables saving.
func (sm *SessionManager) SetStoragePath(path string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.storagePath = path
}

// StoragePath returns the file that Save writes to.
func (sm *SessionManager) StoragePath() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.storagePath
}

// Save compacts the active conversation and writes all conversations to the storage
// path. It does nothing when no storage path is set.
func (sm *SessionManager) Save() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.storagePath == "" {
		return nil
	}

	dir := filepath.Dir(sm.storagePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	// Only the active conversation can have changed since the last save; the others
	// were compacted when they were last active.
	if conv, ok := sm.Conversations[sm.ActiveID]; ok {
		conv.Compact()
	}

	data := struct {
		ActiveID      string                   `json:"activeId"`
		Conversations map[string]*Conversation `json:"conversations"`
		Order         []string                 `json:"order"`
		Counter       int                      `json:"counter"`
		DefaultModel  config.ModelRef          `json:"defaultModel"`
	}{
		ActiveID:      sm.ActiveID,
		Conversations: sm.Conversations,
		Order:         sm.Order,
		Counter:       sm.counter,
		DefaultModel:  sm.DefaultModel,
	}

	// Compact encoding: sessions are machine-managed state (history is
	// browsable in-app via Ctrl+Y / /history), so we optimize for smaller
	// files and less marshal CPU over human readability.
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	// Credentials the agent read or printed (API keys, tokens, private keys) are
	// masked on disk; the in-memory conversation is unchanged.
	raw = redact.Bytes(raw)

	tmpFile := sm.storagePath + ".tmp"
	f, err := os.OpenFile(tmpFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		return errors.Join(err, f.Close(), removeTemp(tmpFile))
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close(), removeTemp(tmpFile))
	}
	if err := f.Close(); err != nil {
		return errors.Join(err, removeTemp(tmpFile))
	}
	if err := os.Rename(tmpFile, sm.storagePath); err != nil {
		return errors.Join(err, removeTemp(tmpFile))
	}
	return nil
}

// removeTemp deletes a failed save's temporary file. A file that is already gone
// is not an error; anything else is reported alongside the save failure.
func removeTemp(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

// Active returns the active conversation. If none is active it makes the first in order active, and
// it creates a default conversation when none exist.
func (sm *SessionManager) Active() *Conversation {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if conv, ok := sm.Conversations[sm.ActiveID]; ok {
		return conv
	}
	if len(sm.Order) > 0 {
		sm.ActiveID = sm.Order[0]
		return sm.Conversations[sm.ActiveID]
	}
	return sm.newLocked("default")
}

// New creates an empty conversation with the given name (generated when blank), makes it active and
// returns it. It inherits the model of the active conversation.
func (sm *SessionManager) New(name string) *Conversation {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.newLocked(name)
}

func (sm *SessionManager) newLocked(name string) *Conversation {
	sm.counter++
	id := strconv.Itoa(sm.counter)
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("conversation-%s", id)
	}

	model := sm.DefaultModel
	if sm.ActiveID != "" {
		if cur, ok := sm.Conversations[sm.ActiveID]; ok && cur.Model.Model != "" {
			model = cur.Model
		}
	}
	if model.Model != "" {
		sm.DefaultModel = model
	}

	now := time.Now()
	conv := &Conversation{
		ID:        id,
		Name:      name,
		Model:     model,
		CreatedAt: now,
		UpdatedAt: now,
		// A new conversation has no edits yet, so it needs no repair on load.
		ThinkingKeptValid: true,
		Messages: []chat.Message{
			{
				Role:    "system",
				Content: CurrentSystemPrompt(),
			},
		},
	}

	sm.Conversations[id] = conv
	sm.Order = append(sm.Order, id)
	sm.ActiveID = id
	return conv
}

// Resume makes the conversation matching query active and returns it. The query can be a
// conversation ID, a 1-based position in List order, or a name, matched case-insensitively.
func (sm *SessionManager) Resume(query string) (*Conversation, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("conversation id or name is required")
	}

	if conv, ok := sm.Conversations[query]; ok {
		sm.ActiveID = conv.ID
		return conv, nil
	}

	if idx, err := strconv.Atoi(query); err == nil && idx >= 1 && idx <= len(sm.Order) {
		id := sm.Order[idx-1]
		if conv, ok := sm.Conversations[id]; ok {
			sm.ActiveID = conv.ID
			return conv, nil
		}
	}

	var matches []*Conversation
	for _, id := range sm.Order {
		conv := sm.Conversations[id]
		if strings.EqualFold(conv.Name, query) {
			matches = append(matches, conv)
		}
	}
	if len(matches) == 1 {
		sm.ActiveID = matches[0].ID
		return matches[0], nil
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("multiple conversations named %q, specify by ID", query)
	}

	queryLower := strings.ToLower(query)
	for _, id := range sm.Order {
		conv := sm.Conversations[id]
		if strings.Contains(strings.ToLower(conv.Name), queryLower) {
			matches = append(matches, conv)
		}
	}
	if len(matches) == 1 {
		sm.ActiveID = matches[0].ID
		return matches[0], nil
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("ambiguous conversation query %q, specify by ID", query)
	}

	return nil, notFoundError(fmt.Sprintf("conversation %q not found", query))
}

// List returns the conversations in display order.
func (sm *SessionManager) List() []*Conversation {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	list := make([]*Conversation, 0, len(sm.Order))
	for _, id := range sm.Order {
		if conv, ok := sm.Conversations[id]; ok {
			list = append(list, conv)
		}
	}
	return list
}

// SetDefaultModel sets the model that new conversations start with.
func (sm *SessionManager) SetDefaultModel(model config.ModelRef) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.DefaultModel = model
}

// Delete removes the conversation with the given ID. If it was active, the first remaining
// conversation becomes active, or a new default one is created.
func (sm *SessionManager) Delete(id string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, ok := sm.Conversations[id]; !ok {
		return notFoundError("conversation " + id + " not found")
	}

	delete(sm.Conversations, id)

	newOrder := make([]string, 0, len(sm.Order))
	for _, oId := range sm.Order {
		if oId != id {
			newOrder = append(newOrder, oId)
		}
	}
	sm.Order = newOrder

	if sm.ActiveID == id {
		if len(sm.Order) > 0 {
			sm.ActiveID = sm.Order[0]
		} else {
			sm.newLocked("default")
		}
	}
	return nil
}

// Rename gives the conversation with the given ID a new name. The name is trimmed and must not be
// empty.
func (sm *SessionManager) Rename(id string, newName string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	newName = strings.TrimSpace(newName)
	if newName == "" {
		return errors.New("conversation name cannot be empty")
	}

	conv, ok := sm.Conversations[id]
	if !ok {
		return notFoundError("conversation " + id + " not found")
	}

	conv.Name = newName
	conv.AutoTitled = false
	conv.UpdatedAt = time.Now()
	return nil
}

// GenerateConversationTitle derives a short title from the first line of prompt, truncated to about
// 42 characters.
func GenerateConversationTitle(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "New conversation"
	}
	lines := strings.Split(prompt, "\n")
	firstLine := strings.TrimSpace(lines[0])
	firstLine = strings.TrimLeft(firstLine, "#-*>`~ ")
	firstLine = strings.TrimSpace(firstLine)
	if firstLine == "" {
		firstLine = prompt
	}
	const maxLen = 42
	runes := []rune(firstLine)
	if len(runes) > maxLen {
		trimmed := string(runes[:maxLen])
		if lastSpace := strings.LastIndex(trimmed, " "); lastSpace > 20 {
			trimmed = trimmed[:lastSpace]
		}
		return strings.TrimSpace(trimmed) + "..."
	}
	return firstLine
}

// MaybeAutoTitle sets the name from prompt when the conversation still has its default or generated
// name. The name it sets is marked as automatic, so it can later be replaced by a generated title.
func (c *Conversation) MaybeAutoTitle(prompt string) {
	if c.Name == "default" || strings.HasPrefix(c.Name, "conversation-") {
		c.Name = GenerateConversationTitle(prompt)
		c.AutoTitled = true
		c.UpdatedAt = time.Now()
	}
}

// ErrConversationNotFound matches, via errors.Is, every lookup of a conversation
// that does not exist.
var ErrConversationNotFound = errors.New("conversation not found")

// notFoundError keeps the message naming the missing conversation while still
// matching ErrConversationNotFound.
type notFoundError string

func (e notFoundError) Error() string        { return string(e) }
func (e notFoundError) Is(target error) bool { return target == ErrConversationNotFound }
