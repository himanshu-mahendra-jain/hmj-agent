package config

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
)

// Sentinel errors for model lookups; check them with errors.Is.
var (
	ErrUnknownProvider    = errors.New("unknown provider")
	ErrModelNotConfigured = errors.New("model not configured")
	ErrNoModels           = errors.New("no models configured")
)

// Version is the application version reported by --version.
const Version = "1.0.2"

// VersionString returns Version with the VCS revision recorded in the build info, if any,
// e.g. "1.0.0 (1eea0d8)" or "1.0.0 (1eea0d8, dirty)".
func VersionString() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Version
	}
	return formatVersion(Version, info.Settings)
}

// formatVersion appends the short VCS revision and a dirty marker from the build settings to base.
func formatVersion(base string, settings []debug.BuildSetting) string {
	var revision string
	var dirty bool
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if revision == "" {
		return base
	}
	meta := revision[:min(len(revision), 7)]
	if dirty {
		meta += ", dirty"
	}
	return base + " (" + meta + ")"
}

// Config is the parsed configuration file: whether accessibility and high-contrast modes are
// on, and the providers keyed by name.
type Config struct {
	Accessibility bool                `json:"accessibility,omitempty"`
	HighContrast  bool                `json:"highContrast,omitempty"`
	Providers     map[string]Provider `json:"providers"`
}

// Provider is an API endpoint with its base URL, API key, wire protocol and the models it serves.
type Provider struct {
	BaseURL string  `json:"baseUrl"`
	APIKey  Secret  `json:"apiKey"`
	API     string  `json:"api,omitempty"`
	Models  []Model `json:"models"`
}

// Wire protocol identifiers for Provider.API.
const (
	APIResponses   = "responses"   // OpenAI Responses API (POST /responses)
	APICompletions = "completions" // Legacy OpenAI Chat Completions (POST /chat/completions)
	APIAnthropic   = "anthropic"   // Anthropic Messages API (POST /v1/messages)
	APIGemini      = "gemini"      // Google Gemini Interactions API (POST /v1beta/interactions)
)

// geminiHost is the hostname of Google's native Gemini API. Its OpenAI-compat
// endpoint lives under the same host with an /openai path segment, and the
// Interactions API lives under the same host at /v1beta/interactions.
const geminiHost = "generativelanguage.googleapis.com"

// Sources reported by Provider.WireAPISource. The chat package adds "probed" for a protocol
// found by probing the endpoint.
const (
	SourceExplicit = "explicit" // the provider's "api" field
	SourceURL      = "url"      // a rule on the base URL
	SourceDefault  = "default"  // nothing matched; only this source may be probed
)

// WireAPI resolves the effective wire protocol for the provider. See WireAPISource.
func (p Provider) WireAPI() string {
	api, _ := p.WireAPISource()
	return api
}

// WireAPISource resolves the wire protocol and reports where it came from. An explicit
// "api" field wins (case-insensitive) and source is "explicit". When it is empty the base
// URL is inspected: a Google Generative Language URL selects Gemini, or Chat Completions when
// it targets the /openai compat path (which only supports chat completions); an Anthropic base
// URL or a /messages suffix selects Anthropic; a /chat/completions suffix selects Chat
// Completions; each of these has source "url". Anything else defaults to the OpenAI Responses
// API with source "default", the only case where the endpoint may be probed. Only the
// canonical identifiers are accepted; there are no legacy aliases.
func (p Provider) WireAPISource() (api, source string) {
	switch strings.ToLower(strings.TrimSpace(p.API)) {
	case APIResponses:
		return APIResponses, SourceExplicit
	case APICompletions:
		return APICompletions, SourceExplicit
	case APIAnthropic:
		return APIAnthropic, SourceExplicit
	case APIGemini:
		return APIGemini, SourceExplicit
	case "":
		trimmed := strings.ToLower(strings.TrimRight(p.BaseURL, "/"))
		switch {
		case strings.Contains(trimmed, geminiHost):
			if strings.Contains(trimmed, "/openai") {
				return APICompletions, SourceURL
			}
			return APIGemini, SourceURL
		case strings.Contains(trimmed, "anthropic") || strings.HasSuffix(trimmed, "/messages"):
			return APIAnthropic, SourceURL
		case strings.HasSuffix(trimmed, "/chat/completions") || strings.HasSuffix(trimmed, "/completions"):
			return APICompletions, SourceURL
		}
		return APIResponses, SourceDefault
	default:
		// Fallback for directly constructed Provider structs (e.g. in test suites).
		// Production configurations loaded from disk are strictly validated by
		// LoadConfig to reject any unrecognized API wire protocols.
		return APIResponses, SourceExplicit
	}
}

// Model is a model offered by a provider. ID is the name sent to the API and Name is an optional
// display label.
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// ContextWindow is the most input tokens the model accepts. When set, older
	// history is condensed before a request is estimated to fill most of it.
	ContextWindow int `json:"contextWindow,omitempty"`
	// MaxOutputTokens caps the tokens the model may generate per response. Unset, Anthropic uses
	// its default of 32000 and the other protocols send no limit.
	MaxOutputTokens int `json:"maxOutputTokens,omitzero"`
	// Effort sets how much reasoning the model spends: low, medium, high, xhigh or max. Unset
	// means the provider default.
	Effort string `json:"effort,omitempty"`
	// Thinking controls reasoning. "" or "adaptive" (the default) requests reasoning from the
	// provider and keeps it in the transcript; "off" sends no reasoning parameters, for models or
	// compatible endpoints that reject them.
	Thinking string `json:"thinking,omitempty"`
}

// ThinkingEnabled reports whether reasoning parameters should be sent for the model. It is true
// unless the model sets thinking to "off".
func (m Model) ThinkingEnabled() bool {
	return m.Thinking != "off"
}

// DisplayName returns Name when it is set and ID otherwise.
func (m Model) DisplayName() string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

// ModelRef identifies a model by its provider name and model ID.
type ModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// DefaultSessionPath returns the path to the sessions file.
// If HMJ_SESSIONS is explicitly set, it takes precedence.
// Otherwise, it computes a deterministic project-scoped storage path under
// ~/.hmj/projects/<project-slug>/sessions.json based on the current workspace root.
func DefaultSessionPath() string {
	if path := os.Getenv("HMJ_SESSIONS"); path != "" {
		return path
	}

	root := findProjectRoot()
	if realRoot, err := filepath.EvalSymlinks(root); err == nil && realRoot != "" {
		root = realRoot
	}

	hash := sha256.Sum256([]byte(root))
	slug := fmt.Sprintf("%s-%x", sanitizeProjectSlug(filepath.Base(root)), hash[:6])

	configPath := DefaultConfigPath()
	dir := filepath.Dir(configPath)
	return filepath.Join(dir, "projects", slug, "sessions.json")
}

// findProjectRoot discovers the root of the project by checking for a .git entry
// (directory for standard clones, or pointer file for git worktrees/submodules)
// starting from the current working directory upwards, or falls back to cwd.
func findProjectRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return wd
}

func sanitizeProjectSlug(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	s := strings.Trim(b.String(), "_-")
	if s == "" {
		return "project"
	}
	return s
}

// DefaultConfigPath returns the config file location: the HMJ_CONFIG path when set, otherwise
// ~/.hmj/config.json.
func DefaultConfigPath() string {
	if path := os.Getenv("HMJ_CONFIG"); path != "" {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "config.json"
	}

	primary := filepath.Join(home, ".hmj", "config.json")
	return primary
}

// ErrEmptyConfig reports a config file that exists but holds nothing.
var ErrEmptyConfig = errors.New("config file is empty")

// LoadConfig reads and validates the config file at path.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("config file not found: %w", err)
		}
		return Config{}, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Config{}, fmt.Errorf("%s: %w", path, ErrEmptyConfig)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// validEfforts are the reasoning effort levels a model may set.
var validEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// validThinking are the values a model's thinking option may take.
var validThinking = []string{"", "adaptive", "off"}

// validate checks that every provider has a base URL and a known wire protocol, and that every
// model's effort and output limit are valid.
func (c Config) validate() error {
	for _, name := range slices.Sorted(maps.Keys(c.Providers)) {
		p := c.Providers[name]
		if p.BaseURL == "" {
			return errors.New("provider " + name + " has no baseUrl")
		}
		if api := strings.ToLower(strings.TrimSpace(p.API)); api != "" {
			switch api {
			case APIResponses, APICompletions, APIAnthropic, APIGemini:
			default:
				return fmt.Errorf("provider %s has unknown api %q (valid options: %s, %s, %s, %s)", name, p.API, APIResponses, APICompletions, APIAnthropic, APIGemini)
			}
		}
		for _, m := range p.Models {
			if m.Effort != "" && !slices.Contains(validEfforts, m.Effort) {
				return fmt.Errorf("provider %s model %s has unknown effort %q (valid options: %s)", name, m.ID, m.Effort, strings.Join(validEfforts, ", "))
			}
			if !slices.Contains(validThinking, m.Thinking) {
				return fmt.Errorf("provider %s model %s has unknown thinking %q (valid options: adaptive, off)", name, m.ID, m.Thinking)
			}
			if m.MaxOutputTokens < 0 {
				return fmt.Errorf("provider %s model %s has negative maxOutputTokens %d", name, m.ID, m.MaxOutputTokens)
			}
		}
	}
	return nil
}

// CreateStarterConfig writes template to path for a first run, creating the directory with
// mode 0700 and the file with mode 0600. It fills a missing or empty file only: a file with
// content is left untouched and reported as fs.ErrExist.
func CreateStarterConfig(path string, template []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if fi, err := f.Stat(); err != nil || fi.Size() > 0 {
		f.Close()
		return cmp.Or(err, fs.ErrExist)
	}
	// An empty file that already existed keeps its mode from before; the key goes in here.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(template); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// UnsetKeyVars describes each provider whose apiKey names an environment variable ("$NAME")
// that is unset or empty, so requests to it would go out without a key.
func UnsetKeyVars(cfg Config) []string {
	var notes []string
	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		envVar, ok := strings.CutPrefix(string(cfg.Providers[name].APIKey), "$")
		if ok && os.Getenv(envVar) == "" {
			notes = append(notes, fmt.Sprintf("provider %s: apiKey refers to $%s, which is not set", name, envVar))
		}
	}
	return notes
}

// ReloadConfig reloads configuration from path. If loading fails or results
// in no providers, the current configuration is retained and an error is returned.
func ReloadConfig(path string, current Config) (Config, error) {
	newCfg, err := LoadConfig(path)
	if err != nil {
		return current, err
	}
	if len(newCfg.Providers) == 0 {
		return current, errors.New("no providers configured in " + path)
	}
	return newCfg, nil
}

// Resolve returns the provider that ref names, or an error if the provider or model is not
// configured.
func (c Config) Resolve(ref ModelRef) (Provider, error) {
	p, _, err := c.ResolveModel(ref)
	return p, err
}

// ResolveModel returns the provider and model that ref names, or an error if either is not
// configured.
func (c Config) ResolveModel(ref ModelRef) (Provider, Model, error) {
	p, ok := c.Providers[ref.Provider]
	if !ok {
		return Provider{}, Model{}, fmt.Errorf("%w: %s", ErrUnknownProvider, ref.Provider)
	}

	for _, model := range p.Models {
		if model.ID == ref.Model {
			return p, model, nil
		}
	}

	return Provider{}, Model{}, fmt.Errorf("%w: %s/%s", ErrModelNotConfigured, ref.Provider, ref.Model)
}

// DefaultModel returns the first model in AllModels order, or an error when no models are
// configured.
func (c Config) DefaultModel() (ModelRef, error) {
	all := c.AllModels()
	if len(all) > 0 {
		return all[0], nil
	}
	return ModelRef{}, ErrNoModels
}

// AllModels lists every configured model, with providers in sorted name order.
func (c Config) AllModels() []ModelRef {
	providers := slices.Sorted(maps.Keys(c.Providers))

	var list []ModelRef
	for _, pName := range providers {
		p := c.Providers[pName]
		for _, m := range p.Models {
			list = append(list, ModelRef{Provider: pName, Model: m.ID})
		}
	}
	return list
}

// FindModel resolves a user query to one model. The query may be a 1-based index into AllModels, a
// provider/model pair, a model ID or a display name. Matching is case-insensitive, and an ambiguous
// query is an error.
func (c Config) FindModel(query string) (ModelRef, error) {
	query = strings.TrimSpace(query)
	all := c.AllModels()
	if len(all) == 0 {
		return ModelRef{}, ErrNoModels
	}

	if ref, ok := findModelByIndex(all, query); ok {
		return ref, nil
	}
	if ref, ok := findModelByProviderModel(all, query); ok {
		return ref, nil
	}
	if ref, ok, err := uniqueModelMatch(matchModelsByID(all, query), query, "ambiguous model"); ok || err != nil {
		return ref, err
	}
	if ref, ok, err := uniqueModelMatch(c.matchModelsByDisplayName(all, query), query, "ambiguous model name"); ok || err != nil {
		return ref, err
	}
	if ref, ok, err := uniqueModelMatch(c.matchModelsBySubstring(all, query), query, "ambiguous query"); ok || err != nil {
		return ref, err
	}

	return ModelRef{}, fmt.Errorf("no model matching %q found", query)
}

// findModelByIndex returns the model at the 1-based index named by query, if query is one.
// The whole query must be the number: "2fast/gpt" names a model, not index 2.
func findModelByIndex(all []ModelRef, query string) (ModelRef, bool) {
	if idx, err := strconv.Atoi(query); err == nil && idx >= 1 && idx <= len(all) {
		return all[idx-1], true
	}
	return ModelRef{}, false
}

// findModelByProviderModel returns the model whose "provider/model" equals query, ignoring case.
func findModelByProviderModel(all []ModelRef, query string) (ModelRef, bool) {
	for _, ref := range all {
		if strings.EqualFold(ref.Provider+"/"+ref.Model, query) {
			return ref, true
		}
	}
	return ModelRef{}, false
}

// matchModelsByID returns the models whose model ID equals query, ignoring case.
func matchModelsByID(all []ModelRef, query string) []ModelRef {
	var matches []ModelRef
	for _, ref := range all {
		if strings.EqualFold(ref.Model, query) {
			matches = append(matches, ref)
		}
	}
	return matches
}

// matchModelsByDisplayName returns the models whose configured display name equals query,
// ignoring case.
func (c Config) matchModelsByDisplayName(all []ModelRef, query string) []ModelRef {
	var matches []ModelRef
	for _, ref := range all {
		if _, m, err := c.ResolveModel(ref); err == nil && m.Name != "" {
			if strings.EqualFold(m.Name, query) {
				matches = append(matches, ref)
			}
		}
	}
	return matches
}

// matchModelsBySubstring returns the models whose "provider/model" or display name contains
// query, ignoring case.
func (c Config) matchModelsBySubstring(all []ModelRef, query string) []ModelRef {
	var matches []ModelRef
	queryLower := strings.ToLower(query)
	for _, ref := range all {
		fullName := strings.ToLower(ref.Provider + "/" + ref.Model)
		var nameLower string
		if _, m, err := c.ResolveModel(ref); err == nil && m.Name != "" {
			nameLower = strings.ToLower(m.Name)
		}
		if strings.Contains(fullName, queryLower) || (nameLower != "" && strings.Contains(nameLower, queryLower)) {
			matches = append(matches, ref)
		}
	}
	return matches
}

// uniqueModelMatch returns the sole match. It reports ok=false with no error when nothing
// matched, and an error naming every candidate, prefixed by kind, when several did.
func uniqueModelMatch(matches []ModelRef, query, kind string) (ModelRef, bool, error) {
	switch {
	case len(matches) == 1:
		return matches[0], true, nil
	case len(matches) > 1:
		return ModelRef{}, false, fmt.Errorf("%s %q matched: %s", kind, query, modelRefList(matches))
	}
	return ModelRef{}, false, nil
}

// modelRefList renders models as a comma-separated "provider/model" list.
func modelRefList(matches []ModelRef) string {
	var list []string
	for _, m := range matches {
		list = append(list, m.Provider+"/"+m.Model)
	}
	return strings.Join(list, ", ")
}

// ResolveAPIKey expands a value of the form $NAME to that environment variable. Any other value is
// returned unchanged. The name is looked up directly, never passed to a shell, so a value such
// as "$(cmd)" names a (missing) variable rather than running cmd.
func ResolveAPIKey(value Secret) string {
	if after, ok := strings.CutPrefix(string(value), "$"); ok {
		return os.Getenv(after)
	}
	return string(value)
}

// APIKeyEnvName returns the environment variable that a value of the form $NAME names,
// or "" for a literal key.
func APIKeyEnvName(value Secret) string {
	if after, ok := strings.CutPrefix(string(value), "$"); ok {
		return after
	}
	return ""
}

// Secret holds a credential such as an API key. Printing, logging or encoding it
// shows "[redacted]" instead of the value; ResolveAPIKey returns the real key.
type Secret string

const redactedSecret = "[redacted]"

func (s Secret) masked() string {
	if s == "" {
		return ""
	}
	return redactedSecret
}

// String masks the secret for fmt verbs such as %v and %s.
func (s Secret) String() string { return s.masked() }

// GoString masks the secret for %#v.
func (s Secret) GoString() string { return strconv.Quote(s.masked()) }

// LogValue masks the secret in log/slog records.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.masked()) }

// MarshalJSON masks the secret when encoded; decoding reads the real value.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(s.masked())), nil }

// SecurityWarnings lists the ways the config at path could expose an API key: the
// file is readable by other users while it holds a literal key (not a $VAR
// reference), or a provider sends a key over plain HTTP to a host other than this
// machine.
func SecurityWarnings(path string, cfg Config) []string {
	var warnings []string
	names := slices.Sorted(maps.Keys(cfg.Providers))
	literal := slices.ContainsFunc(names, func(name string) bool {
		key := cfg.Providers[name].APIKey
		return key != "" && !strings.HasPrefix(string(key), "$")
	})
	if fi, err := os.Stat(path); err == nil && literal && fi.Mode().Perm()&0o077 != 0 {
		warnings = append(warnings, fmt.Sprintf("%s holds an API key and is readable by other users (mode %04o); run: chmod 600 %s",
			path, fi.Mode().Perm(), path))
	}
	for _, name := range names {
		p := cfg.Providers[name]
		u, err := url.Parse(p.BaseURL)
		if err != nil || !strings.EqualFold(u.Scheme, "http") || ResolveAPIKey(p.APIKey) == "" || isLoopbackHost(u.Hostname()) {
			continue
		}
		warnings = append(warnings, fmt.Sprintf("provider %s sends its API key unencrypted to %s; use an https:// baseUrl", name, u.Host))
	}
	return warnings
}

// isLoopbackHost reports whether host names this machine.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// ErrProviderAPISet reports that the provider already has an "api" field.
var ErrProviderAPISet = errors.New("provider already sets \"api\"")

// SetProviderAPI writes "api": api into the named provider's object in the config file at
// path, so a protocol found by probing is not probed again. Everything else in the file is
// kept byte for byte: the user's key order, spacing and indentation. The provider must exist
// and must not already set "api". The rewritten file must load as a valid config before it
// replaces the original, which keeps its permissions.
func SetProviderAPI(path, provider, api string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	offset, err := providerObjectStart(data, provider)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	// Insert the member right after the opening brace, on its own line when the object's
	// members are on their own lines, with their indentation.
	rest := data[offset:]
	indent, ownLine := memberIndent(rest)
	trailing := ","
	if next := bytes.TrimLeft(rest, " \t\r\n"); len(next) > 0 && next[0] == '}' {
		trailing = "" // the object was empty
	}
	var out bytes.Buffer
	out.Write(data[:offset])
	if ownLine {
		fmt.Fprintf(&out, "\n%s%q: %q%s", indent, "api", api, trailing)
	} else {
		fmt.Fprintf(&out, " %q: %q%s", "api", api, trailing)
	}
	out.Write(rest)

	var cfg Config
	// An existing "api" member was refused above, so a failure here is the file's own: the
	// partly decoded provider holds the inserted "api" and says nothing about it.
	if err := json.Unmarshal(out.Bytes(), &cfg); err != nil {
		return fmt.Errorf("%s: rewritten config does not parse: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("%s: rewritten config is invalid: %w", path, err)
	}
	return writeFilePreservingMode(path, out.Bytes())
}

// providerObjectStart returns the byte offset just past the opening brace of the provider's
// object in data, and ErrProviderAPISet when that object already has an "api" member.
func providerObjectStart(data []byte, provider string) (int, error) {
	escaped := strings.NewReplacer("~", "~0", "/", "~1").Replace(provider)
	objectPtr := jsontext.Pointer("/providers/" + escaped)
	apiPtr := objectPtr + "/api"
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	offset := -1
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, err
		}
		switch {
		case tok.Kind() == '{' && dec.StackPointer() == objectPtr:
			offset = int(dec.InputOffset())
		case tok.Kind() == '"' && dec.StackPointer() == apiPtr:
			return 0, ErrProviderAPISet
		}
	}
	if offset < 0 {
		return 0, fmt.Errorf("provider %q not found", provider)
	}
	return offset, nil
}

// memberIndent looks at what follows an object's opening brace: when the first member starts
// on a new line it returns that line's indentation and true, otherwise "" and false.
func memberIndent(rest []byte) (string, bool) {
	i := 0
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
		i++
	}
	if i >= len(rest) || (rest[i] != '\n' && rest[i] != '\r') {
		return "", false
	}
	for i < len(rest) && (rest[i] == '\n' || rest[i] == '\r') {
		i++
	}
	start := i
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
		i++
	}
	return string(rest[start:i]), true
}

// writeFilePreservingMode replaces the file at path atomically, keeping its permissions
// (0600 for a file that does not exist).
func writeFilePreservingMode(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
