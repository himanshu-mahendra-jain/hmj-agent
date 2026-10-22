# HMJ Agent

> A minimal coding agent for the terminal, written in pure Go with the standard library and official `golang.org/x` modules. It runs on Linux.

## Quick start

1. Build and install: `make build` installs the binary to `~/.local/bin/hmj`.
2. Run `hmj`. On the first run it writes `~/.hmj/config.json` from [config.example.json](config.example.json) and exits.
3. Set the key variables the file names, such as `export ANTHROPIC_API_KEY=...`, then run `hmj` again.

## Core principles

1. **Native.** Pure Go with the standard library and official `golang.org/x` modules, and no third-party dependencies. The binary is statically linked (`CGO_ENABLED=0`).
2. **Unrestricted by design.** Built for a dedicated VM. File tools accept any relative, absolute or `~` path, and `run_command` runs any command in any directory (`workdir`). There is no path sandbox or command blocklist.
3. **Always expanded.** Nothing is folded or hidden. Model reasoning, tool parameters, visual diffs and command output stream live to the terminal.
4. **Instant halt, signal safety.** A raw termios listener stops token streaming or a running tool at once on `Esc` or `Ctrl+C`. hmj traps `SIGINT`, `SIGTERM`, `SIGHUP` and `SIGQUIT` to restore the terminal and save sessions.
5. **Multi-line input.** Bracketed paste captures multi-line prompts, code snippets and dragged file paths without submitting early. The prompt carries OSC 133 marks, so terminals with shell integration (kitty, Ghostty, WezTerm, iTerm2) can move the cursor on click and jump between prompts.
6. **Multiple protocols and models.** Supports the OpenAI Responses API (`POST /responses`), the legacy OpenAI Chat Completions API (`POST /chat/completions`), the Anthropic Messages API (`POST /v1/messages`), and the Google Gemini Interactions API (`POST /v1beta/interactions`, with thought-signature round-tripping). Switch models on the fly with `Ctrl+O`.
7. **Sticky model, per-project sessions.** The active model and sessions persist per project under `~/.hmj/projects/<slug>/sessions.json`. The project path is hashed to pick the file, so tabs in different repos do not collide. Writes are atomic (`fsync`), with `.corrupt` backups. Override with `HMJ_SESSIONS`.
8. **Fast streaming.** The SSE reader reuses buffers from a `sync.Pool`, and accumulators use `strings.Builder`, so token streaming does not allocate quadratically.
9. **Provider-side compaction past 400K tokens.** A conversation is compacted only once its prompt passes 400K tokens, and only after a turn ends: a turn is never cut, and its requests go out even past the limit. For Claude the compact request is the turn's own request plus the compaction field, so it reads the prompt cache. It runs while you type the next prompt; if you send it first, hmj waits with a spinner (`Esc` or `Ctrl+C` stops it, and it is tried again after the next turn). Claude uses Anthropic's on-demand compaction and OpenAI Responses uses `POST /responses/compact`. Gemini and Chat Completions have no compaction API, so the model writes a summary in one ordinary request, which is also the fallback when a provider rejects the compact endpoint (HTTP 400 or 404). The compaction replaces all history after the system prompt, and images or documents in it may survive only as the summary describes them. Nothing else is pruned or summarized, so the transcript is append-only between compactions. Token counts reported by the provider are the baseline for the size estimate. A model whose window is under 400K is not compacted earlier, so a long conversation on it can fail on size; start a new one with `Ctrl+N`. A tool result is capped at 320KB, and a request the provider rejects as over the context limit is sent again without the largest result of the current step, which stays replaced by a note so the saved conversation can still be sent.
10. **Retries.** Randomized exponential backoff handles HTTP `429`, `408`, `409`, `529` (overloaded) and `5xx`. It honours `Retry-After` and reset hints in the body (such as "try again in 4m39s"), and fails fast on other `4xx` errors. Rate-limit, server, timeout and overload errors streamed inside a response are retried too, until output has started, as is a Claude response that ends before its stop reason; one that ends so after output is an error, never a complete reply. A response that delivers no bytes for 120 seconds is aborted as stalled. Errors show the provider's request ID when it sends one.
11. **Process hygiene.** Commands run in their own process group (`Setpgid: true`) with 64KB output limits. They inherit hmj's whole environment, API keys included, since hmj is meant for a dedicated VM.
12. **Workspace guidelines.** hmj reads `AGENTS.md`, or `README.md` if there is none, up to the first 160 lines, to keep context small and cache-friendly. Guidelines must never exceed 160 lines.
13. **Accessibility and clean code.** Respects `NO_COLOR` and offers `--accessible`, with ASCII borders and text badges (`[OK]`, `[NOTE]`, `[ERROR]`). It uses modern Go (`encoding/json/v2`, `math/rand/v2`, `netip`, `slices`, `maps`, `cmp.Or`, range-over-int), with no ignored errors, wrapped error chains (`%w`) and co-located unit tests.
14. **Sequential tools.** Mutating tools (`edit_file`, `write_file`, `run_command`) run one at a time, not in the background. Local I/O takes under a millisecond, so sequential runs avoid torn writes and test/edit races, avoid recovery loops from phantom build failures, and avoid speculative reads that pollute context. This keeps the 50–90% discounted KV-cache prefixes valid across all providers.

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

## Build and development

* **Build flags:** `-s -w` strips debug data, `-trimpath` removes host paths, and `CGO_ENABLED=0` makes the binary static and libc-free.
* **Testing:**
  - Unit tests sit beside the code (`./internal/...`), and integration tests live in `./tests`.
  - `make e2e` drives the built binary in a pseudo terminal against a scripted mock provider, and checks the rendered screen with a built-in terminal emulator. `make e2e-live` adds one turn against a real provider.
  - Also covered: native Go fuzzing (`make fuzz`), multi-iteration race stress (`make race`), randomized execution (`-shuffle=on`), parent context cancellation, subprocess tree termination, retry and backoff, multi-protocol streaming, SSE buffer pooling, and conversation persistence.

## CLI flags

- `--version`, `-v`: print the version and exit.
- `--help`, `-h`: print the shortcuts and banner cheat sheet.
- `--health`: check latency and health of all configured providers and models.
- `--accessible`, `--accessibility`, `-a`: accessibility mode. `ACCESSIBILITY=1` or `"accessibility": true` also work. Output is append-only, with no cursor movement or redraws:
  - the spinner prints `[working]` lines;
  - menus are numbered lists: type a number, type text to filter, `r <n> <name>` renames, `d <n>` deletes, and an empty line or `q` cancels;
  - a line ending in `\` continues onto the next prompt line;
  - slash commands replace keyboard shortcuts;
  - `[RESULT name]` blocks hold tool output and remediation hints.
- `--high-contrast`: bright, bold palette with no dim text, underlined removed lines and bold `+`/`-` markers. `HIGH_CONTRAST=1` or `"highContrast": true` also work. Flags win over the config file.
- `--no-color`: disable all ANSI color styling, following the [NO_COLOR](https://no-color.org) standard.
- `--strict`: refuse to start, and ignore config reloads, when the config has a security warning. Warnings cover a file readable by other users that holds a literal API key, and a key sent over plain `http://` to a host other than this machine. Without `--strict` these are printed as warnings.

## Configuration

### First run

On the first run, hmj writes a starter `~/.hmj/config.json` (mode 0600) from [config.example.json](config.example.json) and exits. Set the key variables the file names, then run hmj again. Paths can be changed with `HMJ_CONFIG` (config) and `HMJ_SESSIONS` (sessions). Set `HMJ_LOG` to a file path for an opt-in JSON-lines diagnostic log of tool runs, model steps, retries, config reloads and session saves.

### Files next to the config

- `audit.log`: one JSON line per tool run (tool, arguments, duration, output size, error). It rotates to `audit.log.1` at 10MB. `HMJ_AUDIT_LOG` moves it, or turns it off when set to `off`.
- `backups/`: the previous content of every file that `edit_file` or `write_file` changes, named `<timestamp>_<path with / as %>`. The newest 200 (up to 256MB) are kept.
- `fetch/`: the full text of each page `fetch_url` cut short, named `<timestamp>_<host and path>.txt`. The newest 100 (up to 256MB) are kept.

### API keys

hmj is meant for a dedicated VM, so nothing is masked: the saved session file, the audit and diagnostic logs, and provider errors keep credentials as the agent saw them. Keep credentials that matter, such as git credentials, outside the VM. Use `"apiKey": "$ENV_VAR"` to keep keys out of the file. A literal key should live in a file only you can read (`chmod 600`). Requests never follow redirects, so a key is only sent to the configured `baseUrl`.

### Protocol (`api`)

The optional `"api"` field selects the protocol: `"responses"`, `"completions"`, `"anthropic"` or `"gemini"`. It may be omitted.

- A known base URL selects the protocol. `generativelanguage.googleapis.com` selects `gemini` (or `completions` on its `/openai` path). An Anthropic host or a `/messages` suffix selects `anthropic`. A `/chat/completions` suffix selects `completions`.
- Otherwise, as with a plain `/v1` base, hmj probes the endpoint once with an empty `{}` POST. It tries the Responses, Anthropic Messages, Chat Completions and Gemini Interactions routes in that order, five seconds each. The first route whose error answer shows it exists is used, and hmj writes it into the config as that provider's `"api"`, keeping the rest of the file as you wrote it. The probe never runs again; delete the field to re-detect. If no route answers, Responses is used for that run.
- An explicit `"api"` is never probed.

### Model settings

Each model in a provider's `models` list can set the options below. For example: `{ "id": "<model-id>", "maxOutputTokens": 64000, "effort": "high" }`.

- `"maxOutputTokens"`: the most tokens one response may generate. Anthropic always sends a limit and defaults to `32000`, so large `write_file` calls are not cut off. Other protocols send a limit only when this is set (`max_output_tokens`, `max_tokens`, or Gemini's `max_output_tokens`).
- `"effort"`: how much reasoning the model spends, one of `low`, `medium`, `high`, `xhigh` or `max`. Anthropic sends it as `output_config.effort`, and OpenAI Responses sends it as `reasoning.effort`. Gemini maps `low` and `medium` to the same thinking levels, and `high`, `xhigh` and `max` to `high`. Unset means the provider default. Gemini needs no other change: a `baseUrl` ending in `/v1beta` is used as is.
- `"thinking"`: `"adaptive"` (the default) requests reasoning from the provider. Set `"off"` for a model or compatible endpoint that rejects reasoning parameters, such as a non-reasoning OpenAI model. Those receive no reasoning fields, except that Anthropic still sends a set `effort` as `output_config.effort`.
- `"speed"`: `"fast"` requests Anthropic fast mode (`speed: "fast"`), which gives faster output at premium pricing on the Claude models that offer it. Unset means standard speed, and the other protocols ignore it. Fast mode has its own rate limit, and its `429`s are retried like any other. The turn footer says `fast mode` when the API reports (in `usage.speed`) that the turn ran fast, or `fast mode in 2 of 3 steps` when only some steps did.

### Reasoning

Reasoning is kept in the transcript and sent back, so multi-step tool turns stay valid: Anthropic thinking blocks with their signatures, and OpenAI reasoning items with their encrypted content. Claude accepts a thinking block only while everything before it is unchanged, so hmj never edits sent history; a compaction is the only rewrite, and it replaces everything before it. Reasoning summaries stream live. If a model declines a request under its safety classifier, hmj prints a `[the model declined this request ...]` notice that names the category when the provider gives one.

## Shortcuts and commands

| Shortcut | Command | Action |
|---|---|---|
| `Ctrl+N` | `/new` | Clear the screen and start a new conversation (keeps the active model; earlier ones stay in `/resume`) |
| `Ctrl+R` | `/resume` | Open the conversation manager (switch, `Ctrl+E` rename, `Ctrl+D` delete) |
| `Ctrl+O` | `/models` | Open the model selector (type to filter, `↑`/`↓`, `Enter`) |
| `Ctrl+Y` | `/history` | Browse turns and copy a prompt, AI response or tool actions to the clipboard |
| `↑` / `↓` | — | Cycle prompt history (recalls previous prompts, restores the draft) |
| `Esc` / `Ctrl+C` | — | Halt the running agent or tool immediately |
| `Ctrl+D` | — | Exit HMJ Agent (saves session state) |
| — | `/health` | Check latency and health of all configured providers and models |
| — | `/shortcuts` | Show the shortcuts and commands cheat sheet |

- The prompt uses standard shell editing keys: `Ctrl+A`/`Ctrl+E`, `Ctrl+U`/`Ctrl+K`, `Ctrl+W`, `Ctrl+←`/`Ctrl+→` (or `Alt+←`/`Alt+→`, `Alt+B`/`Alt+F`), `Alt+Backspace`, and `Alt+D` / `Ctrl+Delete`.
- Typing `/` at an empty prompt opens a searchable command palette. Other input, including file paths such as `/etc/...`, goes straight to the model.
- `Ctrl+Y` (or `/history`) opens the turn browser. It shows full transcripts, including partial runs and tool output, and copies prompts, responses or whole turns to the clipboard. It uses OSC 52, which works over SSH, or `wl-copy`, `xclip` or `xsel`.

## The 7 core tools

1. **`read_file`**: streams files line by line, with slices by `offset`, `limit` and `numbered`, from any path on the machine. Without a `limit`, a call returns at most 2000 lines and 256KB, and a longer read ends with a trailer giving the `offset` to continue from. An explicit `limit` is honoured up to the 256KB cap.
2. **`edit_file`**: surgical substring replacement (`old_string` → `new_string`), with unified visual diffs and word-level highlighting.
3. **`write_file`**: creates or replaces files, creating directories as needed (`MkdirAll`).
4. **`list_files`**: lists files recursively, skipping `.git` and respecting `.gitignore`.
5. **`run_command`**: runs any shell command (optional `workdir`, default the current directory) in its own process group (`Setpgid: true`). Subtrees are cancelled on timeout or halt, and output is capped at 64KB. It inherits your toolchains, paths and proxies.
6. **`web_search`**: searches DuckDuckGo and Wikipedia over a shared pooled connection, with a 15s timeout, fallback between backends, and safe context cancellation.
7. **`fetch_url`**: fetches any URL, including `localhost` dev servers inside the VM, over an isolated connection pool. It asks for Markdown first (`Accept: text/markdown`) and converts HTML to text, fetches GitHub `blob` pages as the raw file, decodes base64 sources such as googlesource `?format=TEXT`, and indents JSON. HTML pages keep only their `<main>` or `<article>` content, or drop navigation, footers and sidebars. Output is capped at 120 lines (`max_lines`, up to 1000) and 64KB; a page cut short is saved whole to `~/.hmj/fetch/` for `read_file` or `grep`. Each request waits up to 20s (`timeout_seconds`, up to 120), a redirect to another host is noted, and public pages are kept for 15 minutes (local hosts are fetched every time). Its description points the model to `llms.txt`, `llms-full.txt` and `.md` page variants.

> Always inspect and change files with the native tools (`read_file`, `edit_file`, `write_file`). Never edit files through `run_command` with `python3`, `sed`, `awk` or bash heredocs.

## Security: dedicated VM

HMJ Agent is an unrestricted autonomous coding harness, with no path sandbox, command filter or network block. Run it inside a **dedicated virtual machine** (for example any Linux distribution on KVM or Firecracker).

1. **Shared workspace:** mount the project directory into the VM, and run HMJ inside the guest.
2. **Git on the host:** keep commits, pushes, and SSH and GPG keys on the host machine.
3. **Review before commit:** inspect changes with `git diff` and `git status`, and run security scans on the host before committing.

## License

This project is licensed under the [MIT License](LICENSE).
