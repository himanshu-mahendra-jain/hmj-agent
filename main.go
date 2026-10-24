package main

import (
	"bufio"
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"hmj-agent/internal/agent"
	"hmj-agent/internal/audit"
	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/logging"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal"
	"hmj-agent/internal/terminal/theme"
	"hmj-agent/internal/tools"
)

// starterConfig is written to the config path on a first run, when no config exists yet.
//
//go:embed config.example.json
var starterConfig []byte

// slashCommandActions maps typed slash commands onto the shortcut actions
// that share their handlers.
var slashCommandActions = map[string]terminal.ShortcutAction{
	"/new":     terminal.ActionNew,
	"/resume":  terminal.ActionResume,
	"/models":  terminal.ActionModels,
	"/history": terminal.ActionHistory,
}

// forcedFlags records the display modes set on the command line. An explicit flag wins
// over the config file, including across config reloads.
type forcedFlags struct {
	accessible   bool
	highContrast bool
	// strict turns config security warnings into errors.
	strict bool
}

// app is the state of one interactive run: the loaded config, the active model,
// the client and session manager, and the flags that survive config reloads.
type app struct {
	configPath string
	cfg        config.Config
	modelRef   config.ModelRef
	client     *chat.Client
	runner     *agent.Runner
	sm         *session.SessionManager
	forced     forcedFlags
	// cfgModTime and cfgSize stamp the config file as last loaded, so reloads
	// can skip files that have not changed.
	cfgModTime time.Time
	cfgSize    int64

	// ctx is cancelled at shutdown; every turn runs under it so a signal stops
	// streaming and kills running tool processes.
	ctx    context.Context
	cancel context.CancelFunc
	// busy is held while a turn mutates the active conversation.
	// Shutdown takes it before saving, so the save never races those writes.
	busy chan struct{}
	// lastSaveErr is the last session save failure shown, so a persistent
	// failure (e.g. a read-only filesystem) is reported once, not every turn.
	lastSaveErr string
	// compaction is the compaction started when the last turn ended, running while the next
	// input is typed; finishCompaction applies it before that input is handled.
	compaction *agent.Compaction
	// afterTurn is set when a turn has run since the last prompt, which is then set off from it
	// by a divider.
	afterTurn bool
}

// shutdownGrace bounds how long a signal waits for a running turn to unwind.
const shutdownGrace = 5 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	forced, code, exit := applyFlags(args, stdout, stderr)
	if exit {
		return code
	}

	closeLog, logErr := logging.Init(os.Getenv("HMJ_LOG"))
	if logErr != nil {
		fmt.Fprintf(stderr, "%s %v\n", theme.Muted("[warning: diagnostic log disabled:]"), logErr)
	} else {
		defer closeLog()
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "unknown: " + err.Error()
	}
	logging.L().Info("start", "version", config.VersionString(), "cwd", cwd)

	a, err := loadApp(forced)
	if err != nil {
		fmt.Fprintln(stderr, err)
		agent.PrintHint(stderr, err)
		return 1
	}
	defer a.cancel()
	defer openAuditTrail(stderr)()

	// Stamp the config as loaded before reading it so a concurrent edit
	// between stat and read still triggers a reload on the next prompt.
	a.stampConfigLoaded()
	a.sm = session.LoadSessionManager(config.DefaultSessionDir(), a.modelRef)

	terminal.SaveOrigTermios()
	defer terminal.Restore()
	terminal.DisableInterruptKey()

	stopSignals := a.installSignalHandler()
	defer stopSignals()

	conv := a.activateStartupConversation()
	printWelcome(conv)

	a.repl(bufio.NewReader(stdin))
	a.finishCompaction()
	a.saveSession()
	if err := a.sm.Close(); err != nil {
		logging.L().Warn("conversation lock not released", "error", err.Error())
	}
	tools.Jobs.StopAll()
	return 0
}

// openAuditTrail starts the audit log of tool runs, the backups of edited files and the saved
// text of truncated fetch_url pages and run_command outputs, all kept next to the config file. HMJ_AUDIT_LOG moves the audit log, or turns it off when
// set to "off". It returns a func that closes the log.
func openAuditTrail(stderr io.Writer) func() {
	dataDir := filepath.Dir(config.DefaultConfigPath())
	tools.SetBackupDir(filepath.Join(dataDir, "backups"))
	tools.SetFetchDir(filepath.Join(dataDir, "fetch"))
	tools.SetCommandLogDir(filepath.Join(dataDir, "commands"))
	chat.SetImageDir(filepath.Join(dataDir, "images"))

	auditPath := cmp.Or(os.Getenv("HMJ_AUDIT_LOG"), filepath.Join(dataDir, "audit.log"))
	if auditPath == "off" {
		return func() {}
	}
	closeAudit, err := audit.Init(auditPath, config.VersionString())
	if err != nil {
		fmt.Fprintf(stderr, "%s %v\n", theme.Muted("[warning: audit log disabled:]"), err)
		return func() {}
	}
	return func() {
		if err := closeAudit(); err != nil {
			fmt.Fprintf(stderr, "%s %v\n", theme.Muted("[warning: audit log not closed cleanly:]"), err)
		}
	}
}

// applyFlags processes the command-line flags in order. Informational flags, --health and
// an unknown argument finish the run: exit reports that, and code is the process status.
func applyFlags(args []string, stdout, stderr io.Writer) (forced forcedFlags, code int, exit bool) {
	for _, arg := range args {
		switch arg {
		case "--help", "-h":
			agent.PrintBanner()
			return forced, 0, true
		case "--version", "-v":
			fmt.Fprintf(stdout, "HMJ Agent %s\n", config.VersionString())
			return forced, 0, true
		case "--health":
			return forced, runHealth(stdout, stderr), true
		case "--accessible", "-a":
			theme.SetAccessibility(true)
			forced.accessible = true
		case "--high-contrast":
			theme.SetHighContrast(true)
			forced.highContrast = true
		case "--no-color":
			theme.SetEnabled(false)
		case "--strict":
			forced.strict = true
		default:
			fmt.Fprintf(stderr, "unknown argument %q; run hmj --help for the shortcuts, or see the README for flags\n", arg)
			return forced, 2, true
		}
	}
	return forced, 0, false
}

// runHealth probes every configured model and writes the report to stdout.
func runHealth(stdout, stderr io.Writer) int {
	cfg, err := loadConfig(config.DefaultConfigPath())
	if err != nil {
		fmt.Fprintln(stderr, err)
		agent.PrintHint(stderr, err)
		return 1
	}
	warnConfigSecurity(config.DefaultConfigPath(), cfg)
	client := chat.NewClient(cfg)
	client.OnProtocolDetected = func(provider, api string, saved bool) {
		fmt.Fprintln(stderr, theme.Muted(agent.ProtocolDetectedNotice(provider, api, saved)))
	}
	if err := agent.HandleHealthTo(cfg, client, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		agent.PrintHint(stderr, err)
		return 1
	}
	return 0
}

// starterConfigCreated reports that a first run wrote a starter config which the user
// must now fill in.
type starterConfigCreated struct{ path string }

func (e *starterConfigCreated) Error() string {
	return "created a starter config at " + e.path + "\n" +
		"Add your provider, model and API key there (or set the $ANTHROPIC_API_KEY / $OPENAI_API_KEY /\n" +
		"$GEMINI_API_KEY variables it reads keys from), remove the providers you do not use, then run\n" +
		"hmj again."
}

// loadConfig reads the config at path. When the file is missing or empty it writes a
// starter config there and returns a *starterConfigCreated telling the user to fill it in.
// Other errors are prefixed with "config:".
func loadConfig(path string) (config.Config, error) {
	cfg, err := config.LoadConfig(path)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, config.ErrEmptyConfig) {
		return config.Config{}, fmt.Errorf("config: %w", err)
	}
	if createErr := config.CreateStarterConfig(path, starterConfig); createErr != nil {
		return config.Config{}, fmt.Errorf("config: %w (creating a starter config failed: %v)", err, createErr)
	}
	return config.Config{}, &starterConfigCreated{path: path}
}

// loadApp reads the config and builds the client and runner. The error is
// ready to print: it names the config file or the missing default model.
func loadApp(forced forcedFlags) (*app, error) {
	configPath := config.DefaultConfigPath()
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, err
	}
	if err := checkConfigSecurity(configPath, cfg, forced.strict); err != nil {
		return nil, err
	}
	for _, note := range config.UnsetKeyVars(cfg) {
		fmt.Fprintf(os.Stderr, "%s %s\n", theme.Caution("config warning:"), note)
	}

	applyDisplayConfig(forced, cfg)

	if len(cfg.Providers) == 0 {
		return nil, fmt.Errorf("no providers configured in %s", configPath)
	}

	modelRef, err := cfg.DefaultModel()
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", configPath, err)
	}

	client := chat.NewClient(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	return &app{
		configPath: configPath,
		cfg:        cfg,
		modelRef:   modelRef,
		client:     client,
		runner:     agent.NewRunner(client, cfg),
		forced:     forced,
		ctx:        ctx,
		cancel:     cancel,
		busy:       make(chan struct{}, 1),
	}, nil
}

// errInsecureConfig is returned for a config with security warnings under --strict.
var errInsecureConfig = errors.New("insecure config rejected by --strict")

// checkConfigSecurity prints the config's security warnings, or returns them as an
// error under --strict.
func checkConfigSecurity(path string, cfg config.Config, strict bool) error {
	if !strict {
		warnConfigSecurity(path, cfg)
		return nil
	}
	if warnings := config.SecurityWarnings(path, cfg); len(warnings) > 0 {
		return fmt.Errorf("%w:\n  - %s", errInsecureConfig, strings.Join(warnings, "\n  - "))
	}
	return nil
}

// warnConfigSecurity prints each security warning about the config on stderr.
func warnConfigSecurity(path string, cfg config.Config) {
	for _, w := range config.SecurityWarnings(path, cfg) {
		fmt.Fprintf(os.Stderr, "%s %s\n", theme.Caution("security warning:"), w)
	}
}

// applyDisplayConfig sets the accessibility and high-contrast modes from the config file.
// Explicit flags always win; the modes are assigned unconditionally so a reload can also
// switch them off when neither the flag nor the config asks for them.
func applyDisplayConfig(forced forcedFlags, cfg config.Config) {
	theme.SetAccessibility(forced.accessible || cfg.Accessibility)
	theme.SetHighContrast(forced.highContrast || cfg.HighContrast)
}

// stampConfigLoaded records the config file's current modification time and size.
func (a *app) stampConfigLoaded() {
	if fi, statErr := os.Stat(a.configPath); statErr == nil {
		a.cfgModTime, a.cfgSize = fi.ModTime(), fi.Size()
	}
}

// reloadConfig picks up edits to the config file. A missing or invalid file keeps
// the current configuration.
func (a *app) reloadConfig() {
	// Cheap mtime/size guard: skip the full read+parse on every prompt
	// when the file has not changed since the last successful load.
	if fi, statErr := os.Stat(a.configPath); statErr == nil &&
		fi.ModTime().Equal(a.cfgModTime) && fi.Size() == a.cfgSize {
		return
	}
	newCfg, err := config.ReloadConfig(a.configPath, a.cfg)
	if err != nil {
		// A deleted config keeps the current one silently; it would otherwise
		// warn on every prompt.
		if !errors.Is(err, os.ErrNotExist) {
			logging.L().Warn("config reload failed", "path", a.configPath, "error", err.Error())
			fmt.Fprintf(os.Stderr, "%s %v\n", theme.Muted("[warning: config reload failed:]"), err)
		}
		return
	}
	if err := checkConfigSecurity(a.configPath, newCfg, a.forced.strict); err != nil {
		logging.L().Warn("config reload rejected", "path", a.configPath, "error", err.Error())
		fmt.Fprintf(os.Stderr, "%s %v\n", theme.Muted("[warning: config reload ignored:]"), err)
		a.stampConfigLoaded() // report a rejected file once, not before every prompt
		return
	}
	a.cfg = newCfg
	a.client.Config = newCfg
	applyDisplayConfig(a.forced, newCfg)
	if def, err := newCfg.DefaultModel(); err == nil {
		a.modelRef = def
	}
	a.stampConfigLoaded()
	logging.L().Info("config reloaded", "path", a.configPath)
}

// saveSession writes the active conversation and the session state, reporting a failure
// without stopping the run.
func (a *app) saveSession() {
	dir := config.DefaultSessionDir()
	if err := a.sm.Save(); err != nil {
		logging.L().Error("session save failed", "dir", dir, "error", err.Error())
		if msg := err.Error(); msg != a.lastSaveErr {
			fmt.Fprintf(os.Stderr, "%s %v\n", theme.Muted("[session save failed:]"), err)
			a.lastSaveErr = msg
		}
		return
	}
	if a.lastSaveErr != "" {
		fmt.Fprintln(os.Stderr, theme.Muted("[session saved again]"))
		a.lastSaveErr = ""
	}
	logging.L().Debug("session saved", "dir", dir)
}

// installSignalHandler makes SIGINT, SIGTERM, SIGHUP and SIGQUIT save the session,
// restore the terminal and exit with the signal's status. A second signal during
// cleanup exits immediately. It returns the function that stops signal delivery.
func (a *app) installSignalHandler() (stop func()) {
	sigChan := make(chan os.Signal, 2)
	done := make(chan struct{})
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		select {
		case sig := <-sigChan:
			go exitOnSecondSignal(sigChan, done)
			a.shutdown(sig)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sigChan)
		close(done)
	}
}

// exitOnSecondSignal exits immediately with 128+N if another signal arrives while
// shutdown is still cleaning up.
func exitOnSecondSignal(sigChan <-chan os.Signal, done <-chan struct{}) {
	select {
	case sig := <-sigChan:
		terminal.Restore()
		os.Exit(signalExitCode(sig))
	case <-done:
	}
}

// shutdown stops a running turn (cancelling its stream and killing tool process
// groups), waits up to shutdownGrace for it to unwind, then saves the session,
// restores the terminal and exits. The session is saved only once the turn has
// released its slot: a turn that is still running may be appending to the
// conversation, so saving then could persist a half-written transcript. It must
// exit here rather than return: the REPL may be blocked reading stdin, which no
// context can interrupt.
func (a *app) shutdown(sig os.Signal) {
	a.cancel()
	if a.waitForTurn(shutdownGrace) {
		a.saveSession()
	}
	tools.Jobs.StopAll()
	terminal.Restore()
	fmt.Println()
	os.Exit(signalExitCode(sig))
}

// waitForTurn waits up to grace for the busy slot, which a running turn holds. It reports whether the
// slot was acquired; the caller then owns the conversation. When it is not, the turn may still be
// appending to the conversation, so the caller must not save the session.
func (a *app) waitForTurn(grace time.Duration) bool {
	select {
	case a.busy <- struct{}{}:
		return true
	case <-time.After(grace):
		logging.L().Warn("turn did not stop before shutdown; session not saved", "grace", grace)
		fmt.Fprintln(os.Stderr, theme.Caution("A running turn did not stop in time; the session was not saved."))
		return false
	}
}

// signalExitCode maps a terminating signal to its conventional 128+N exit status.
func signalExitCode(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 128 + int(syscall.SIGINT)
}

// activateStartupConversation makes the active conversation usable with the current
// config and loads its prompt history.
func (a *app) activateStartupConversation() *session.Conversation {
	conv := a.sm.Active()
	if _, err := a.cfg.Resolve(conv.Model); err != nil {
		conv.Model = a.modelRef
	}
	a.sm.SetDefaultModel(conv.Model)
	syncPromptHistory(conv)
	return conv
}

// printWelcome prints the banner and, when the conversation has messages, a summary of it.
func printWelcome(conv *session.Conversation) {
	agent.PrintBanner()
	if count := conv.UserMessageCount(); count > 0 {
		msgStr := "messages"
		if count == 1 {
			msgStr = "message"
		}
		fmt.Printf("%s %s %s\n\n",
			theme.Muted("resumed conversation:"),
			theme.Accent(conv.Name),
			theme.Muted(fmt.Sprintf("(%d %s)", count, msgStr)),
		)
		agent.PrintConversationHistory(conv)
	} else {
		fmt.Println()
	}
}

// repl reads and dispatches input until reading fails.
func (a *app) repl(reader *bufio.Reader) {
	for {
		conv := a.promptModel()

		// A background job of this conversation that ends while the prompt is empty starts a
		// turn, so the model picks the work back up.
		stopWake := make(chan struct{})
		input, action, err := terminal.ReadSeamlessInput(reader, tools.Jobs.Wake(conv.ScratchDir, stopWake))
		close(stopWake)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "%s %v\n", theme.Muted("[input closed:]"), err)
			}
			return
		}
		a.dispatch(input, action, conv, reader)
		if a.ctx.Err() != nil {
			// Shutdown is under way; leave the terminal to it instead of
			// putting it back into raw mode for another prompt.
			select {}
		}
	}
}

// promptModel selects the active conversation for the next prompt, falling back to
// the default model when its model is no longer configured, and prints the prompt, after a blank
// line and a divider when a turn has run since the last one.
func (a *app) promptModel() *session.Conversation {
	if a.afterTurn {
		a.afterTurn = false
		fmt.Printf("\n%s\n", terminal.TurnDivider())
	}
	conv := a.sm.Active()
	if _, err := a.cfg.Resolve(conv.Model); err != nil {
		conv.Model = a.modelRef
		a.sm.SetDefaultModel(conv.Model)
	}
	modelLabel, effort := conv.Model.Provider+"/"+conv.Model.Model, ""
	if p, m, err := a.cfg.ResolveModel(conv.Model); err == nil {
		if m.Name != "" {
			modelLabel += " (" + m.Name + ")"
		}
		effort = theme.Muted(theme.ASCII(" · ")+"effort: ") + theme.Info(chat.EffortLabel(p, m))
	}
	var jobs string
	if n := tools.Jobs.Running(conv.ScratchDir); n > 0 {
		jobs = theme.Muted(theme.ASCII(" · ")) + theme.Info(fmt.Sprintf("%d background job%s running", n, map[bool]string{true: "s"}[n > 1]))
	}
	fmt.Printf("%s %s%s%s%s\n", theme.Muted("[model:"), theme.Info(modelLabel), effort, jobs, theme.Muted("]"))
	fmt.Print(terminal.PromptPrefix() + theme.Reset())
	return conv
}

// dispatch runs one line of input: a shortcut or slash command, a local command,
// or a prompt sent to the model.
func (a *app) dispatch(input string, action terminal.ShortcutAction, conv *session.Conversation, reader *bufio.Reader) {
	a.finishCompaction()
	// A keyboard shortcut wins; otherwise a typed slash command maps onto the same action.
	if action == terminal.ActionNone {
		action = slashCommandActions[strings.TrimSpace(input)]
	}
	if action == terminal.ActionWake {
		a.runTurn(conv, "")
		return
	}
	if action != terminal.ActionNone {
		a.handleAction(action, conv, reader)
		return
	}

	if input == "" {
		return
	}

	switch strings.TrimSpace(input) {
	case "/shortcuts":
		agent.PrintShortcuts()
	case "/health":
		a.reloadConfig()
		if err := agent.HandleHealth(a.cfg, a.client); err != nil {
			reportError("/health", err)
		}
	case "/jobs":
		if err := agent.HandleJobs(reader); err != nil {
			reportError("/jobs", err)
		}
	default:
		terminal.AddPromptHistory(input)
		a.runTurn(conv, input)
	}
}

// runTurn sends input to the model as one turn, then starts the compaction it may need and
// saves the session. An empty input is a turn for background jobs that have ended.
func (a *app) runTurn(conv *session.Conversation, input string) {
	a.reloadConfig()
	a.busy <- struct{}{}
	defer func() { <-a.busy }()
	if err := a.runner.Run(a.ctx, conv, input); err != nil {
		fmt.Fprintln(os.Stderr)
		reportError("request to "+conv.Model.Provider+"/"+conv.Model.Model, err)
	}
	a.compaction = a.runner.StartCompaction(a.ctx, conv)
	a.saveSession()
	a.afterTurn = true
}

// finishCompaction waits for the compaction started after the last turn, if any, and puts the
// compacted conversation in place before anything else reads or changes it, holding the busy slot
// so a shutdown does not save the conversation halfway.
func (a *app) finishCompaction() {
	if a.compaction == nil {
		return
	}
	a.busy <- struct{}{}
	defer func() { <-a.busy }()
	a.compaction.Finish()
	a.compaction = nil
	a.saveSession()
}

// handleAction runs a keyboard shortcut or the slash command mapped to it.
func (a *app) handleAction(action terminal.ShortcutAction, conv *session.Conversation, reader *bufio.Reader) {
	switch action {
	case terminal.ActionNew:
		if err := agent.HandleNew(a.sm); err != nil {
			reportError("/new", err)
		}
		syncPromptHistory(a.sm.Active())
	case terminal.ActionResume:
		a.reloadConfig()
		if err := agent.HandleResume(a.cfg, a.sm, reader); err != nil {
			reportError("/resume", err)
		}
		syncPromptHistory(a.sm.Active())
	case terminal.ActionModels:
		a.reloadConfig()
		if err := agent.HandleModels(a.cfg, a.sm, reader); err != nil {
			reportError("/models", err)
		}
	case terminal.ActionHistory:
		if err := agent.HandleHistory(conv, reader); err != nil {
			reportError("/history", err)
		}
	}
}

// reportError prints the error of a failed command or request, naming what failed so
// the message stands on its own, then a remediation hint in accessible mode. A provider's
// error body and headers are in the message, so it cannot drive the terminal.
func reportError(what string, err error) {
	fmt.Fprintf(os.Stderr, "%s %s\n", theme.Danger(what+" failed:"), theme.Sanitize(err.Error()))
	agent.PrintHint(os.Stderr, err)
}

// syncPromptHistory loads the user's prompts from c into the editor's up-arrow history.
func syncPromptHistory(c *session.Conversation) {
	if c == nil {
		terminal.SetPromptHistory(nil)
		return
	}
	var prompts []string
	for _, m := range c.Messages {
		if m.Role == "user" && m.Content != "" {
			prompts = append(prompts, m.Content)
		}
	}
	terminal.SetPromptHistory(prompts)
}
