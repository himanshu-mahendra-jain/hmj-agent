# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/2.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changes

* A model may set `"speed": "fast"` to request Anthropic fast mode. The turn footer says `fast mode` when the API reports that the turn ran at fast speed.
* The turn footer counts prompt-cache reads for Anthropic and Gemini models; it showed them only for the OpenAI protocols.
* Conversations are compacted only once the prompt passes 400K tokens, after a turn ends and never inside one, for every protocol. Claude conversations use Anthropic's on-demand compaction, OpenAI Responses conversations use `POST /responses/compact`, and Gemini and Chat Completions conversations, or a provider that rejects the compact endpoint, get a summary the model writes. The compaction runs while the next prompt is typed; a prompt sent before it ends waits for it, and `Esc` or `Ctrl+C` stops it.
* Tool output is no longer pruned between turns, older turns are no longer summarized after six turns, and the `contextWindow` model setting is removed. The transcript stays as sent until it is compacted, so Claude thinking blocks stay valid. Session files written by earlier versions are no longer repaired on load: a pruned conversation keeps thinking blocks Claude may reject with `Invalid signature in thinking block`, and tool calls left without a result stay open, which providers reject. Start a new conversation for either.
* Credentials are no longer masked as `[REDACTED]` in the saved session file, the audit and diagnostic logs, or provider errors, since hmj is meant for a dedicated VM. A resumed conversation is now exactly the one the model produced: masking changed the text of signed Claude thinking blocks and compactions that quoted a credential, and the API then rejected every later request of that conversation.
* `run_command` passes hmj's whole environment to commands, including variables ending in `_API_KEY`, `_TOKEN`, `_SECRET` or `_PASSWORD` and those an `"apiKey": "$VAR"` names, and hmj no longer marks itself non-dumpable. Tools such as `gh` can use a token from the environment.
* A compacted conversation keeps its title.
* Anthropic requests send `anthropic-version: 2026-10-21`.
* The conversation title request to a Claude model asks for low effort and allows 256 output tokens, as for the other reasoning protocols, instead of turning thinking off with a 40-token cap: Claude 5 models think regardless, and the thinking could use up the cap before any title was written.
* The starter config written on the first run is updated, with API keys read from environment variables.
* The turn footer's output speed and time to first token now count a step that only calls tools from when its tool call starts, on the Anthropic and Chat Completions protocols. Before, the whole step counted as output time, so a tool-using turn showed a speed far too low.
* In a multi-step turn the footer shows the cached tokens of the latest step, matching the input it shows, instead of adding up every step's cached tokens.
* Each model step in the diagnostic log (`HMJ_LOG`) records its time to first output (`ttft`) and cached tokens.
* Model requests no longer ask for gzip-compressed responses: an event stream gains little from compression, and a compressing proxy may hold the stream back before passing it on.
* The diagnostic log (`HMJ_LOG`) records each model response: how long its headers took, the HTTP version, any content encoding, the provider's request ID, and for Anthropic the organization and workspace the API key ran in.
* Anthropic, Gemini and OpenAI requests list map keys, including each tool's schema keys, in sorted order. They came out in a random order on each run, so the request bytes the API caches changed from run to run.
* A Claude response whose stream closes before its stop reason is no longer taken as complete, so a tool call with cut-off arguments, such as a half-written `write_file`, never runs. It is retried when no output has been shown, and reported as an error otherwise.
* Rate-limit (`rate_limit_error`) and timeout (`timeout_error`) errors that the Anthropic API reports inside a stream are retried before output starts, as overload and server errors were and as their HTTP 429 and 504 forms are.
* Claude models before the Claude 5 family, such as Opus 4.6 and 4.8, are no longer supported.
* A model set to `"thinking": "off"` gets the same 256-token limit for its conversation title request as other models, instead of 40 tokens. Many such models reason anyway, Claude among them, and the reasoning used up the 40 tokens, so the conversation kept its automatic name. An Anthropic model with `"off"` and an `effort` set asks for low effort for the title.
* HTTP 409 (`conflict_error`) is retried, as the official Anthropic SDKs do.
* An Anthropic error response is shown as its type and message, such as `HTTP 402: billing_error: Your credit balance is too low.`, instead of its raw JSON. HTTP error responses and errors the API reports inside a stream end with the request ID (`request_id req_…`) that Anthropic support asks for; the diagnostic log (`HMJ_LOG`) has it for every response.
* Error hints: a 403 points at the account's and workspace's access to the model as well as the API key, and 402 (billing) and 413 (request too large) get hints of their own.

## [1.0.4] - 2026-10-14

### Changes

* `--version` takes the version from the release tag the binary was built from. Builds that are not an exact release (between tags, with local changes, or `go run`) report `dev` with their commit, e.g. `dev (2fb9d1a)` (#4).

## [1.0.3] - 2026-10-13

### Changes

* Drop `data:` and `vbscript:` links, like `javascript:` links, when fetching web pages (#2).
* Redact secrets in tool error messages written to the diagnostic log (`HMJ_LOG`), as the tool arguments already were (#2).

## [1.0.2] - 2026-10-13

### Dependencies

* Bump `golang.org/x/term` from 0.46.0 to 0.47.0 (#1, via Dependabot).

## [1.0.1] - 2026-10-10

### New Features

* Initial Release.
