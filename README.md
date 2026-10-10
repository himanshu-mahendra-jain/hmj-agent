# HMJ Agent

> A minimal coding agent harness in pure Go using only the standard library and official `golang.org/x` modules, with zero 3rd-party dependencies, for Linux.

## Core Principles

1. **Native**: Pure Go using only the standard library and official `golang.org/x` modules, with zero 3rd-party dependencies, statically linked (`CGO_ENABLED=0`).
2. **Unrestricted by Design**: Built for a dedicated VM. File tools accept any relative, absolute, or `~` path, and `run_command` runs any command in any directory (`workdir`). No path sandbox or command blocklist.
3. **Always-Expanded Stream**: No collapsed folds or hidden outputs. Model reasoning, tool parameters, visual diffs, and execution output stream live to the terminal.
4. **Instant Halting & Signal Safety**: Raw POSIX termios listener halts token streaming or active tool execution immediately on `Esc` or `Ctrl+C`. Traps `SIGINT`, `SIGTERM`, `SIGHUP`, and `SIGQUIT` to restore terminal state and persist sessions.
5. **Seamless Multi-Line Input**: Bracketed paste mode captures multi-line prompts, code snippets, and drag-and-drop file paths cleanly without premature submission. The prompt carries OSC 133 marks, so terminals with shell integration (kitty, Ghostty, WezTerm, iTerm2) can move the cursor on click and jump between prompts.
6. **Multi-Protocol & Multi-Model**: Supports modern OpenAI Responses API (`POST /responses`), legacy OpenAI Chat Completions (`POST /chat/completions`), and Anthropic Messages (`POST /v1/messages`), and native Google Gemini (`POST /v1beta/interactions`, the Interactions API, with thought-signature round-tripping), with on-the-fly model switching (`Ctrl+O`).
7. **Sticky Model & Workspace-Scoped Sessions**: Active model and sessions persist per project under `~/.hmj/projects/<slug>/sessions.json` via deterministic workspace path hashing, avoiding multi-tab collisions across repos with atomic `fsync` and `.corrupt` backups. Override via `HMJ_SESSIONS`.
8. **High-Performance Streaming**: `sync.Pool` buffer reuse in the SSE reader and incremental `strings.Builder` accumulators eliminate quadratic heap allocations during token streaming.
9. **Dual-Tier Context Compaction**: Between turns, tool outputs are pruned to 10 head + 5 tail lines (keeping the latest 2 in full). The transcript is never rewritten mid-turn, so the provider's prompt cache prefix stays valid across tool steps. Once a conversation passes 6 turns, the model itself is asked to summarize the turns older than the last 3, and its summary takes their place. Token counts reported by the provider are the baseline for the context estimate.
10. **Fault-Tolerant Retry Engine**: Randomized exponential backoff handles HTTP `429`, `408`, `529` (overloaded) and `5xx`, parsing `Retry-After` headers and human-readable body reset hints (`"try again in 4m39s"`) while failing fast on other `4xx` client errors. Overload errors streamed inside a response are retried too, until output has started. A response that delivers no bytes for 120 seconds is aborted as stalled.
11. **Process Hygiene**: `run_command` strips LLM credentials from the child env (names ending in `_API_KEY`, `_TOKEN`, `_SECRET`, `_PASSWORD`, plus every variable the config names as an `"apiKey": "$VAR"`), and hmj marks itself non-dumpable so commands cannot read them back from `/proc/<hmj pid>/environ`, enforcing `Setpgid: true` and 64KB output limits.
12. **Workspace Context Sync**: Discovers `AGENTS.md` (or `README.md`), reading up to the first 160 lines to keep context tight and cache-friendly. Guidelines must never exceed 160 lines.
13. **Accessibility & Clean Standards**: Respects `NO_COLOR` and provides `--accessible` (`-a`) with ASCII borders and explicit status badges (`[OK]`, `[NOTE]`, `[ERROR]`). Idiomatic modern Go (Go 1.22–1.27: `encoding/json/v2`, `math/rand/v2`, `netip`, `slices`, `maps`, `cmp.Or`, range-over-int) with zero error suppression, preserved error chains (`%w`), and co-located in-package unit tests.
14. **Deterministic Sequential Execution**: Mutating tools (`edit_file`, `write_file`, `run_command`) execute synchronously rather than asynchronously in the background. Because local I/O is sub-millisecond, sequential execution eliminates torn writes and test/edit race conditions, prevents token-wasting recovery loops from phantom build failures, avoids context pollution from speculative reads, and preserves immutable 50–90% discounted KV-cache prefixes across all providers.

## Architecture

```text
 ┌─────────────────────────────────────────────────────────────┐
 │                       Linux CLI REPL                        │
 │  - Native bracketed paste (seamless multi-line input)       │
 │  - Raw termios listener (instant halt on Esc / Ctrl+C)      │
 │  - Conversation manager (Ctrl+R) & model switcher (Ctrl+O)  │
 └──────────────────────────────┬──────────────────────────────┘
                                │
 ┌──────────────────────────────▼──────────────────────────────┐
 │                    Autonomous Agent Loop                    │
 │  - Real-time token streaming with live reasoning thoughts   │
 │  - Unfolded, always-expanded tool call diffs & outputs      │
 │  - Multi-step iterative tool execution                      │
 └──────────────┬──────────────────────────────┬───────────────┘
                │                              │
 ┌──────────────▼──────────────┐ ┌─────────────▼───────────────┐
 │       7 Core Tools          │ │     Multi-Wire Protocol     │
 │ - read_file (streaming)     │ │ - Responses API (/responses)│
 │ - write_file (any path)     │ │ - Legacy Chat (/completions)│
 │ - edit_file (surgical diff) │ │ - Anthropic (/v1/messages)  │
 │ - list_files (noise-pruned) │ │ - Gemini (Interactions)     │
 │ - run_command (setpgid)     │ │ - Live reasoning & thinking │
 │ - web_search (pooled HTTP)  │ │ - Streaming tool assembly   │
 │ - fetch_url (localhost ok)  │ │ - Retry & backoff engine    │
 └─────────────────────────────┘ └─────────────────────────────┘
```

## Build & Development

```bash
make
```

* **Build Flags**: `-s -w` strips debug data; `-trimpath` removes host paths; `CGO_ENABLED=0` ensures static libc-free portability.
* **CLI Flags**:
  - `--version`, `-v`: Print current HMJ Agent version and exit.
  - `--help`, `-h`: Print shortcuts and banner cheat sheet.
  - `--health`: Check latency & health of all configured providers and models.
  - `--accessible`, `--accessibility`, `-a`: Enable accessibility mode (`ACCESSIBILITY=1` or `"accessibility": true` also work). Output is append-only, with no cursor movement or redraws: the spinner prints `[working]` lines, menus are numbered lists (type a number, text to filter, `r <n> <name>` to rename, `d <n>` to delete, empty or `q` to cancel), a line ending in `\` continues onto the next prompt line, and keyboard shortcuts are replaced by slash commands. ASCII borders, text badges (`[OK]`, `[ERROR]`), `[RESULT name]` blocks for tool output and remediation hints are used throughout.
  - `--high-contrast`: Bright, bold palette with no dim text, underlined removed lines and bold `+`/`-` markers (`HIGH_CONTRAST=1` or `"highContrast": true` also work). Flags win over the config file.
  - `--no-color`: Disable all ANSI color styling (adhering to the [NO_COLOR](https://no-color.org) standard).
  - `--strict`: Refuse to start (and ignore config reloads) when the config has a security warning: a file readable by other users that holds a literal API key, or a key sent over plain `http://` to a host other than this machine. Without it these are printed as warnings.
* **Test suite**: Comprehensive test matrix with co-located in-package unit tests (`./internal/...`) and integration suites (`./tests`), and an end-to-end suite (`./e2e`, `make e2e`) that drives the built binary in a pseudo terminal against a scripted mock provider, checking the rendered screen through a built-in terminal emulator (`make e2e-live` adds one turn against a real provider): native Go fuzzing (`make fuzz`), multi-iteration race stress (`make race`), randomized execution (`-shuffle=on`), parent context cancellation, subprocess tree termination, retry/backoff engines, multi-protocol streaming, SSE buffer pooling, and conversation persistence.

## Configuration

On the first run, hmj writes a starter `~/.hmj/config.json` (mode 0600) from `config.example.json` and exits; add your provider, model and API key, then run it again (path overridable via `HMJ_CONFIG`; session storage via `HMJ_SESSIONS`). Set `HMJ_LOG` to a file path for an opt-in JSON-lines diagnostic log of tool runs, model steps, retries, config reloads and session saves.

Next to the config file the agent always keeps:

* `audit.log`: one JSON line per tool run (tool, arguments, duration, output size, error), rotated to `audit.log.1` at 10MB. `HMJ_AUDIT_LOG` moves it, or turns it off when set to `off`.
* `backups/`: the previous content of every file `edit_file` or `write_file` changes, named `<timestamp>_<path with / as %>`. The newest 200 (up to 256MB) are kept.

API keys, tokens and private keys are masked as `[REDACTED]` in the saved session file, the audit and diagnostic logs, and provider error messages. The in-memory conversation, and so what the model sees, is unchanged. Use `"apiKey": "$ENV_VAR"` to keep keys out of the config file; keys written literally should be in a file only you can read (`chmod 600`). Provider requests never follow redirects, so a key is only ever sent to the configured `baseUrl`.

The optional `"api"` field selects the wire protocol (`"responses"`, `"completions"`, `"anthropic"`, or `"gemini"`). It may be omitted. When the base URL settles the protocol (`generativelanguage.googleapis.com` selects `gemini`, or `completions` for its `/openai` compat path; an Anthropic host or a `/messages` suffix selects `anthropic`; a `/chat/completions` suffix selects `completions`), that is used. When it does not, as with a plain `/v1` base, hmj probes the endpoint once with an empty `{}` POST to the Responses, Anthropic Messages, Chat Completions and Gemini Interactions routes, in that order, five seconds each. The first route whose error answer shows it exists is used, and hmj writes it into the config file as that provider's `"api"` (keeping the rest of the file as you wrote it), so it is never probed again; delete the field to re-detect. When no route answers, Responses is used for that run. An explicit `"api"` is never probed.

Each model may set `"contextWindow"`, its input limit in tokens (e.g. `{ "id": "gemini-3.8-flash", "contextWindow": 1000000 }`). When set, older turns are summarized, then older tool output pruned, before the conversation is estimated to fill 75% of it. Without it, older turns are summarized only once a conversation passes six turns. The summary is written by the conversation's own model, in one extra request made between turns, and replaces the older turns in every later request; if that request fails, the turns are kept and the summary is tried again next turn.

Each model may also set (e.g. `{ "id": "claude-opus-5-5", "maxOutputTokens": 64000, "effort": "high" }`):

* `"maxOutputTokens"`: the most tokens one response may generate. Anthropic always sends a limit and defaults to `32000` when this is unset, so large `write_file` calls are not cut off. The other protocols send a limit only when this is set (`max_output_tokens`, `max_tokens`, or Gemini's `max_output_tokens`).
* `"effort"`: how much reasoning the model spends, one of `low`, `medium`, `high`, `xhigh` or `max`. Anthropic sends it as `output_config.effort`. OpenAI Responses sends it as `reasoning.effort`. Gemini maps `low` and `medium` to the same thinking levels and `high`, `xhigh` and `max` to `high`. Gemini uses the Interactions API (`POST /v1beta/interactions`) and needs no config change: a `baseUrl` ending in `/v1beta` is used as is. Unset means the provider default.
* `"thinking"`: `"adaptive"` (the default) requests reasoning from the provider; set `"off"` for a model or compatible endpoint that rejects reasoning parameters (for example a non-reasoning OpenAI model or a Claude model before 4.6), which then receives no reasoning fields at all.

Reasoning is kept in the transcript and sent back: Anthropic thinking blocks with their signatures, and OpenAI reasoning items with their encrypted content, so multi-step tool turns stay valid. Reasoning summaries stream live. If a model declines a request under its safety classifier, the agent prints a `[the model declined this request ...]` notice that names the category when the provider gives one.

```json
{
  "accessibility": false,
  "highContrast": false,
  "providers": {
    "openai": {
      "baseUrl": "https://api.openai.com/v1",
      "api": "responses",
      "apiKey": "$OPENAI_API_KEY",
      "models": [
        { "id": "gpt-6-luna" }
      ]
    },
    "gemini": {
      "baseUrl": "https://generativelanguage.googleapis.com/v1beta",
      "api": "gemini",
      "apiKey": "$GEMINI_API_KEY",
      "models": [
        { "id": "gemini-3.8-flash" }
      ]
    }
  }
}
```

## Shortcuts & Commands

| Shortcut | Command | Action |
|---|---|---|
| `Ctrl+N` | `/new` | Clear the screen and start a new conversation (preserves active model; earlier ones stay in `/resume`) |
| `Ctrl+R` | `/resume` | Open conversation manager (switch, `Ctrl+E` rename, `Ctrl+D` delete) |
| `Ctrl+O` | `/models` | Open interactive model selector (type-to-filter, `↑`/`↓`, `Enter`) |
| `Ctrl+Y` | `/history` | Browse turns & copy prompt, AI response, or tool actions to clipboard |
| `↑` / `↓` | — | Cycle prompt history (recalls previous prompts, restores draft) |
| `Esc` / `Ctrl+C` | — | Halt running agent or tool execution immediately |
| `Ctrl+D` | — | Exit HMJ Agent (saves session state) |
| — | `/health` | Check latency & health of all configured providers and models |
| — | `/shortcuts` | Display shortcuts and commands cheat sheet |

* The prompt uses the standard shell editing keys: `Ctrl+A`/`Ctrl+E`, `Ctrl+U`/`Ctrl+K`, `Ctrl+W`, `Ctrl+←`/`Ctrl+→` (or `Alt+←`/`Alt+→`, `Alt+B`/`Alt+F`), `Alt+Backspace`, and `Alt+D` / `Ctrl+Delete`.
* Typing `/` at an empty prompt opens an interactive, searchable command palette. Normal prompt inputs, including file paths (`/etc/...`), pass through directly to the model.
* `Ctrl+Y` (or `/history`) opens the turn browser to inspect full execution transcripts (including partial runs and tool outputs) and copy prompts, responses, or full turns to the clipboard via OSC 52 ANSI escape sequences (working over SSH) or native utilities (`wl-copy`, `xclip`, `xsel`).

## The 7 Core Tools

1. **`read_file`**: Streams files line-by-line with targeted slices (`offset`, `limit`, `numbered`); any path on the machine. Without a `limit` a call returns at most 2000 lines and 256KB; a longer read ends with a trailer giving the `offset` to continue from. An explicit `limit` is honoured up to the 256KB cap.
2. **`edit_file`**: Surgical substring replacement (`old_string` -> `new_string`) with unified visual diffs and word-level highlighting.
3. **`write_file`**: Creates or replaces files with automatic directory creation (`MkdirAll`).
4. **`list_files`**: Recursively lists files while skipping `.git` and respecting `.gitignore` rules.
5. **`run_command`**: Runs any shell command (optional `workdir`, default cwd) with process group isolation (`Setpgid: true`), credential sanitization (`_API_KEY`, `_TOKEN`, `_SECRET`, `_PASSWORD` and config-named key variables), subtree cancellation on timeout/halt, and 64KB output limits. Inherits user toolchains, paths, and proxies.
6. **`web_search`**: Searches documentation across DuckDuckGo and Wikipedia over a shared pooled connection with a 15s timeout, multi-backend fallback, and context-cancellation safety.
7. **`fetch_url`**: Text fetcher for any URL (including `localhost` dev servers inside the VM), converting HTML to plain text capped at 120 lines via an isolated connection pool.

> Always inspect and modify files using native tools (`read_file`, `edit_file`, `write_file`). Never shell out to `python3`, `sed`, `awk`, or bash heredocs via `run_command` to edit files.

## Security & Isolation: Dedicated VM

HMJ Agent is an unrestricted autonomous coding harness (no path sandbox, command filter, or network block) designed to be operated inside a **dedicated Virtual Machine** (e.g. any Linux distribution on KVM/Firecracker):
1. **Shared Workspace**: Mount target project directory into the VM. Run HMJ inside the guest VM.
2. **Host-Side Git**: Keep Git commits, pushes, and SSH/GPG keys on the host machine.
3. **Review Before Commit**: Inspect modified files (`git diff`, `git status`) and run security scans on the host before committing.

## License

This project is licensed under the [MIT License](LICENSE).
