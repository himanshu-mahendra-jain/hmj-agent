# HMJ Agent

> A minimal coding agent for the terminal, written in pure Go with the standard library and official `golang.org/x` modules. It runs on Linux. The design spec, with flowcharts of every flow, is [docs/SPEC.md](docs/SPEC.md).

## Quick start

1. Install the latest release with `curl -fsSL https://raw.githubusercontent.com/himanshu-mahendra-jain/hmj-agent/main/install.sh | sh`, which checks its SHA-256 sum and puts it in `~/.local/bin/hmj` (Linux amd64 or arm64); run it again to update, or set `HMJ_VERSION=v<version>` for a given release. To build from source instead, `make build` installs to the same place.
2. Run `hmj`. On the first run it writes `~/.hmj/config.json` from [config.example.json](config.example.json) and exits.
3. Set the key variables the file names, such as `export ANTHROPIC_API_KEY=...`, then run `hmj` again.

## Core principles

1. **Native.** Pure Go with the standard library and official `golang.org/x` modules, and no third-party dependencies. The binary is statically linked (`CGO_ENABLED=0`).
2. **Unrestricted by design.** Built for a dedicated VM. File tools accept any relative, absolute or `~` path, and `run_command` runs any command in any directory (`workdir`). There is no path sandbox or command blocklist.
3. **Quiet reads, full detail where it counts.** The model's Markdown is rendered line by line as it streams, and in resumed conversations and `/history`: headings, bullets, quotes, rules, tables, and bold, italic, code and links within a line, with code fences shown as written; a web link shows its text, clickable, and a file link its text and path. On a terminal it sits behind a 2-column margin and is word-wrapped to the window, list items hanging under their text (code and tables are not wrapped; accessibility mode keeps lines whole). `Ctrl+Y` copies stay raw Markdown, and with colour off it prints as written. Tool parameters, visual diffs and errors print in full, except that a `run_command` script longer than 4 lines shows its first 3. While the model reasons, the status line shows the latest sentence of its reasoning as the provider sends it, and the transcript keeps `▸ Thought for 8s`. `read_file`, `list_files`, `grep`, `fetch_url` and `web_search`, whose output only the model needs, print one line once they finish, such as `grep: TODO (glob: *.go) · 43 matches in 9 files · 12ms` (a search adds its results' titles), and `run_command` its last 10 output lines. `Ctrl+Y` shows every call and result in full. While the model works or a tool runs, a status line shows the elapsed time, the step, the prompt and output tokens with tokens/s, the tool call the model is writing with its size so far, and a running command's latest output line. Every turn ends with a token and timing footer, a failed one included, ending with the local time the turn was done (`done at 18:01`), then a blank line and a dim rule set it off from the next prompt.
4. **Instant halt, signal safety.** A raw termios listener stops token streaming or a running tool at once on `Esc` or `Ctrl+C`; `Ctrl+B` moves a running command to the background instead. hmj traps `SIGINT`, `SIGTERM`, `SIGHUP` and `SIGQUIT` to restore the terminal and save sessions.
5. **Multi-line input.** Bracketed paste captures multi-line prompts, code snippets and dragged file paths without submitting early. The prompt carries OSC 133 marks, so terminals with shell integration (kitty, Ghostty, WezTerm, iTerm2) can move the cursor on click and jump between prompts.
6. **Multiple protocols and models.** Supports the OpenAI Responses API (`POST /responses`), the legacy OpenAI Chat Completions API (`POST /chat/completions`), the Anthropic Messages API (`POST /v1/messages`), and the Google Gemini Interactions API (`POST /v1beta/interactions`, with thought-signature round-tripping). Switch models on the fly with `Ctrl+O`.
7. **Sticky model, per-project sessions.** Conversations persist per project in `~/.hmj/projects/<slug>/` (the project path is hashed to pick it; `HMJ_SESSIONS` overrides it): one file per conversation in `conversations/<id>.json`, and the last active conversation and model in `state.json`; one left without a prompt is not kept. Writes are atomic (`fsync`), and a damaged file is kept as a `.corrupt` backup. Several hmj can run in one project, for different parts of it: each locks the conversation it has open and writes only that one, so a second hmj starts its own, and `/resume` marks one open elsewhere `[open in another hmj]` and will not open, rename or delete it. Each conversation gets a scratch directory, `scratch/<id>/`, which its system prompt names and `run_command` passes as `$HMJ_SCRATCH`, for temporary files that would otherwise land in `/tmp` or the project; it is deleted with the conversation, or after 7 days unchanged.
8. **Fast streaming.** The SSE reader reuses buffers from a `sync.Pool`, and accumulators use `strings.Builder`, so token streaming does not allocate quadratically.
9. **Provider-side compaction past 400K tokens.** A conversation is compacted only once its prompt passes 400K tokens, and only after a turn ends: a turn is never cut, and its requests go out even past the limit. For Claude the compact request is the turn's own request plus the compaction field, so it reads the prompt cache. It runs while you type the next prompt; if you send it first, hmj waits with a spinner (`Esc` or `Ctrl+C` stops it, and it is tried again after the next turn). Claude uses Anthropic's on-demand compaction and OpenAI Responses uses `POST /responses/compact`. Gemini and Chat Completions have no compaction API, so the model writes a summary in one ordinary request, which is also the fallback when a provider rejects the compact endpoint (HTTP 400 or 404). A conversation the provider finds too long even to compact is summarized from a shortened plain-text copy instead (tool inputs and outputs cut to 1KB, the oldest turns left out if need be), halved until it fits. The compaction replaces all history after the system prompt, and images or documents in it may survive only as the summary describes them. The model no longer sees the turns it replaced, but `Ctrl+Y` still lists them, marked `(compacted)`: their text, tool calls and results are kept beside the session file in `history/<id>.jsonl`, deleted with the conversation. Nothing else is pruned or summarized, so the transcript is append-only between compactions. Token counts reported by the provider are the baseline for the size estimate. On a model whose window is under 400K, the turn the provider refuses as too long is followed by a compaction, and the next prompt continues from the summary. A tool result is capped at 320KB, and a request the provider rejects as over the context limit is sent again without the largest result of the current step, which stays replaced by a note so the saved conversation can still be sent.
10. **Retries.** Randomized exponential backoff handles HTTP `429`, `408`, `409`, `529` (overloaded) and `5xx`. It honours `Retry-After` and reset hints in the body (such as "try again in 4m39s"), and fails fast on other `4xx` errors. Rate-limit, server, timeout and overload errors streamed inside a response are retried too, until output has started, as is a Claude response that ends before its stop reason; one that ends so after output is an error, never a complete reply. A response that delivers no bytes for 120 seconds is aborted as stalled. Errors show the provider's request ID when it sends one.
11. **Process hygiene.** Commands run in their own process group (`Setpgid: true`) with 64KB output limits. They inherit hmj's whole environment, API keys included, since hmj is meant for a dedicated VM.
12. **Workspace guidelines.** hmj reads `AGENTS.md`, or `README.md` if there is none, up to the first 160 lines, to keep context small and cache-friendly. Guidelines must never exceed 160 lines.
13. **Accessibility and clean code.** Respects `NO_COLOR` (any value turns colour off), `FORCE_COLOR` and `CLICOLOR_FORCE` (colour into a pipe), and offers `--accessible`, with ASCII borders and text badges (`[OK]`, `[NOTE]`, `[ERROR]`). It uses modern Go (`encoding/json/v2`, `math/rand/v2`, `netip`, `slices`, `maps`, `cmp.Or`, range-over-int), with no ignored errors (`make check` runs `cmd/droppederr`, nilness and deadcode), wrapped error chains (`%w`) and co-located unit tests.
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
 │  - Real-time token streaming, reasoning in the status line  │
 │  - Full diffs, one-line summaries of what the model reads   │
 │  - Multi-step iterative tool execution                      │
 └──────────────┬──────────────────────────────┬───────────────┘
                │                              │
 ┌──────────────▼──────────────┐ ┌─────────────▼───────────────┐
 │       9 Core Tools          │ │     Multi-Wire Protocol     │
 │ - read_file (streaming)     │ │ - Responses API (/responses)│
 │ - write_file (any path)     │ │ - Legacy Chat (/completions)│
 │ - edit_file (surgical diff) │ │ - Anthropic (/v1/messages)  │
 │ - list_files (noise-pruned) │ │ - Gemini (Interactions)     │
 │ - grep (gitignore-aware)    │ │ - Live reasoning & thinking │
 │ - run_command (setpgid)     │ │ - Streaming tool assembly   │
 │ - web_search (pooled HTTP)  │ │ - Retry & backoff engine    │
 │ - fetch_url (localhost ok)  │ │                             │
 │ - check_command (bg jobs)   │ │                             │
 └─────────────────────────────┘ └─────────────────────────────┘
```

## Build and development

* **Build flags:** `-s -w` strips debug data, `-trimpath` removes host paths, and `CGO_ENABLED=0` makes the binary static and libc-free.
* **Testing:**
  - Tests sit beside the code they cover (`./internal/...`).
  - `make e2e` drives the built binary in a pseudo terminal against a scripted mock provider, and checks the rendered screen with a built-in terminal emulator. `make e2e-live` adds one turn against a real provider.
  - Also covered: native Go fuzzing (`make fuzz`), multi-iteration race stress (`make race`), randomized execution (`-shuffle=on`), parent context cancellation, subprocess tree termination, retry and backoff, multi-protocol streaming, SSE buffer pooling, and conversation persistence.

## CLI flags

- `--version`, `-v`: print the version and exit.
- `--help`, `-h`: print the shortcuts and banner cheat sheet.
- `--health`: check latency and health of all configured providers and models.
- `--accessible`, `-a`: accessibility mode. `ACCESSIBILITY=1` or `"accessibility": true` also work. Output is append-only, with no cursor movement or redraws:
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

On the first run, hmj writes a starter `~/.hmj/config.json` (mode 0600) from [config.example.json](config.example.json) and exits. Set the key variables the file names, then run hmj again. Paths can be changed with `HMJ_CONFIG` (config) and `HMJ_SESSIONS` (the session directory). Set `HMJ_LOG` to a file path for an opt-in JSON-lines diagnostic log of tool runs, model steps, retries, config reloads and session saves.

### Files next to the config

- `audit.log`: one JSON line per tool run (tool, arguments, duration, output size, error). It rotates to `audit.log.1` at 10MB. `HMJ_AUDIT_LOG` moves it, or turns it off when set to `off`.
- `backups/`: the previous content of every file that `edit_file` or `write_file` changes, named `<timestamp>_<path with / as %>`. The newest 200 (up to 256MB) are kept.
- `fetch/`: the full text of each page `fetch_url` cut short, named `<timestamp>_<host and path>.txt`. The newest 100 (up to 256MB) are kept.
- `images/`: the pictures tool results carry, named by their SHA-256; conversations refer to them. The newest 500 (up to 1GB) are kept, and a pruned one is sent as a note.

### API keys

hmj is meant for a dedicated VM, so nothing is masked: the saved session file, the audit and diagnostic logs, and provider errors keep credentials as the agent saw them. Keep credentials that matter, such as git credentials, outside the VM. Use `"apiKey": "$ENV_VAR"` to keep keys out of the file. A literal key should live in a file only you can read (`chmod 600`). Requests never follow redirects, so a key is only sent to the configured `baseUrl`.

### Protocol (`api`)

The optional `"api"` field selects the protocol: `"responses"`, `"completions"`, `"anthropic"` or `"gemini"`. It may be omitted.

- A known base URL selects the protocol. `generativelanguage.googleapis.com` selects `gemini` (or `completions` on its `/openai` path). An Anthropic host or a `/messages` suffix selects `anthropic`. A `/chat/completions` suffix selects `completions`.
- Otherwise, as with a plain `/v1` base, hmj probes the endpoint once with an empty `{}` POST. It tries the Responses, Anthropic Messages, Chat Completions and Gemini Interactions routes in that order, five seconds each. The first route whose error answer shows it exists is used, and hmj writes it into the config as that provider's `"api"`, keeping the rest of the file as you wrote it. The probe never runs again; delete the field to re-detect. If no route answers, Responses is used for that run.
- An explicit `"api"` is never probed.

### Model settings

Each model in a provider's `models` list can set the options below. For example: `{ "id": "<model-id>", "maxOutputTokens": 64000, "effort": "high" }`.

- `"maxOutputTokens"`: the most tokens one response may generate. Anthropic always sends a limit and defaults to `64000`, as Anthropic recommends for agentic work; set it lower for a model whose output limit is smaller. Other protocols send a limit only when this is set (`max_output_tokens`, `max_tokens`, or Gemini's `max_output_tokens`).
- `"effort"`: how much reasoning the model spends, one of OpenAI's levels `none`, `minimal`, `low`, `medium`, `high`, `xhigh` or `max`. OpenAI Responses sends it as `reasoning.effort` and Chat Completions as `reasoning_effort`. Anthropic sends it as `output_config.effort`, with `none` and `minimal` as `low`. Gemini keeps `low` and `medium`, maps `none` and `minimal` to `minimal`, and `high`, `xhigh` and `max` to `high`. Unset means the provider default. The line before each prompt and the `/models` list show what is sent, such as `[model: anthropic/claude-opus-5-5 · effort: high]`, `default`, `high (from xhigh)` for Gemini, or `default (high not sent: thinking off)`. Gemini needs no other change: a `baseUrl` ending in `/v1beta` is used as is.
- `"thinking"`: `"adaptive"` (the default) requests reasoning from the provider. Set `"off"` for a model or compatible endpoint that rejects reasoning parameters, such as a non-reasoning OpenAI model. Those receive no reasoning fields, except that Anthropic still sends a set `effort` as `output_config.effort`.
- `"speed"`: `"fast"` requests Anthropic fast mode (`speed: "fast"`), which gives faster output at premium pricing on the Claude models that offer it. Unset means standard speed, and the other protocols ignore it. Fast mode has its own rate limit, and its `429`s are retried like any other. The turn footer says `fast mode` when the API reports (in `usage.speed`) that the turn ran fast, or `fast mode in 2 of 3 steps` when only some steps did.

### Reasoning

Reasoning is kept in the transcript and sent back, so multi-step tool turns stay valid: Anthropic thinking blocks with their signatures, and OpenAI reasoning items with their encrypted content. Claude accepts a thinking block only while everything before it is unchanged, so hmj never edits sent history; a compaction is the only rewrite, and it replaces everything before it. Reasoning summaries stream into the status line. If a model declines a request under its safety classifier, hmj prints a `[the model declined this request ...]` notice that names the category when the provider gives one.

## Shortcuts and commands

| Shortcut | Command | Action |
|---|---|---|
| `Ctrl+N` | `/new` | Clear the screen and start a new conversation (keeps the active model; earlier ones stay in `/resume`) |
| `Ctrl+R` | `/resume` | Open the conversation manager (switch, `Ctrl+E` rename, `Ctrl+D` delete) |
| `Ctrl+O` | `/models` | Open the model selector (type to filter, `↑`/`↓`, `Enter`) |
| `Ctrl+Y` | `/history` | Browse full turns, partial runs and tool output included, and copy a prompt, response, tool actions or a whole turn to the clipboard (OSC 52, which works over SSH, or `wl-copy`, `xclip` or `xsel`) |
| `↑` / `↓` | — | Cycle prompt history (recalls previous prompts, restores the draft) |
| `Esc` / `Ctrl+C` | — | Halt the running agent or tool immediately |
| `Ctrl+B` | — | Move a running command to the background and end the turn, so you can keep chatting |
| `Ctrl+D` | — | Exit HMJ Agent (saves session state, stops background jobs) |
| — | `/jobs` | List background jobs and stop one |
| — | `/health` | Check latency and health of all configured providers and models |
| — | `/shortcuts` | Show the shortcuts and commands cheat sheet |

- The prompt uses standard shell editing keys: `Ctrl+A`/`Ctrl+E`, `Ctrl+U`/`Ctrl+K`, `Ctrl+W`, `Ctrl+←`/`Ctrl+→` (or `Alt+←`/`Alt+→`, `Alt+B`/`Alt+F`), `Alt+Backspace`, and `Alt+D` / `Ctrl+Delete`. Typing `/` at an empty prompt opens a searchable command palette; other input, including file paths such as `/etc/...`, goes straight to the model.

## The 9 core tools

1. **`read_file`**: streams files line by line, numbered unless `numbered` is false, with slices by `offset` and `limit`, from any path on the machine; a directory's path returns its listing. A PNG, JPEG, GIF or WebP comes back as the picture itself (at most 2000px a side), any other binary file as a one-line description. Without a `limit`, a call returns at most 2000 lines and 256KB, and a longer read ends with a trailer giving the `offset` to continue from. An explicit `limit` is honoured up to the 256KB cap.
2. **`edit_file`**: surgical substring replacement (`old_string` → `new_string`), or several as `edits`, applied in order, all or none, with unified visual diffs and word-level highlighting.
3. **`write_file`**: creates or replaces files, creating directories as needed (`MkdirAll`), and shows the change as a diff against the file's current content (all lines added for a new file).
4. **`list_files`**: lists files recursively, skipping `.git` and respecting `.gitignore`; a `pattern` such as `**/*Test.kt` lists only the files it matches.
5. **`grep`**: searches file contents with a Go regular expression (or `literal` text), in pure Go, skipping `.git`, binary files and what `.gitignore` ignores. It returns the matching files, the matching lines with `context`, or counts per file, filtered by a `glob` such as `*.{ts,tsx}` or `src/**/*Test.kt`, and stops at `head_limit` (default 250) and 64KB.
6. **`run_command`** and **`check_command`**: runs any shell command under `sh -o pipefail` (where `sh` has it; optional `workdir`, default the current directory) in its own process group (`Setpgid: true`). Commands time out after 60s, or `timeout_seconds` up to 30 minutes, and subtrees are cancelled on timeout or halt. With `background`, the call returns at once with a job number and the command runs on, up to 4 hours, its output in `$HMJ_SCRATCH/jobs/`; `Ctrl+B` sends a running command there too. The model is told when a job ends, with its exit code and last lines: in its next tool result while it works, else in a turn that starts by itself at an empty prompt (with text typed, the notice goes with your message). Each conversation numbers its own jobs; `check_command` reports on one of them or stops it, the prompt line counts running jobs, and quitting hmj stops them. stdout and stderr come back as one stream, in the order written; an output over 64KB keeps its first 16KB and last 48KB, and the whole output is saved to `~/.hmj/commands/`. A `sed -i` or `perl -i` outside the scratch directory gets a note pointing to `edit_file`, since its change shows no diff and keeps no backup. It inherits your toolchains, paths and proxies.
7. **`web_search`**: searches DuckDuckGo, falling back to Brave's and Bing's result pages when it blocks the address, then DuckDuckGo instant answers and Wikipedia's article search, over a shared pooled connection with a 15s timeout. `allowed_domains`, `blocked_domains` and `recency` (day to year) narrow it. Results name their source and today's date, show each page's date when the source gives one, and a repeated search is answered from a 15-minute cache.
8. **`fetch_url`**: fetches any URL, including `localhost` dev servers inside the VM, over an isolated connection pool. It asks for Markdown first (`Accept: text/markdown`) and converts HTML to text, fetches GitHub `blob` pages as the raw file, decodes base64 sources such as googlesource `?format=TEXT`, and indents JSON. HTML pages keep only their `<main>` or `<article>` content, or drop navigation, footers and sidebars. Output is capped at 120 lines (`max_lines`, up to 1000) and 64KB; a page cut short is saved whole to `~/.hmj/fetch/` for `read_file` or `grep`. Each request waits up to 20s (`timeout_seconds`, up to 120), a redirect to another host is noted, and public pages are kept for 15 minutes (local hosts are fetched every time). Its description points the model to `llms.txt`, `llms-full.txt` and `.md` page variants.

> Always inspect, search and change files with the native tools (`read_file`, `grep`, `edit_file`, `write_file`). Never edit files through `run_command` with `python3`, `sed`, `awk` or bash heredocs.

## Security: dedicated VM

HMJ Agent is an unrestricted autonomous coding harness, with no path sandbox, command filter or network block. Run it inside a **dedicated virtual machine** (for example any Linux distribution on KVM or Firecracker).

1. **Shared workspace:** mount the project directory into the VM, and run HMJ inside the guest.
2. **Git on the host:** keep commits, pushes, and SSH and GPG keys on the host machine, and before committing review changes there with `git diff` and `git status` and run security scans.

## License

This project is licensed under the [MIT License](LICENSE).
