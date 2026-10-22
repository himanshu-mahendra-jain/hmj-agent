# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/2.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changes

* A model may set `"speed": "fast"` to request Anthropic fast mode. The turn footer says `fast mode` when the API reports that the turn ran at fast speed.
* The turn footer counts prompt-cache reads for Anthropic and Gemini models; it showed them only for the OpenAI protocols.
* Pruning older tool output and summarizing older turns now drop the Claude thinking blocks after the edit, which newer Anthropic accounts reject with `Invalid signature in thinking block` once the history before them changes. Saved conversations that were already pruned lose their thinking blocks once when loaded.
* Anthropic requests send `anthropic-version: 2026-10-21`.
* The conversation title request to a Claude model asks for low effort and allows 256 output tokens, as for the other reasoning protocols, instead of turning thinking off with a 40-token cap: Claude 5 models think regardless, and the thinking could use up the cap before any title was written.
* The starter config written on the first run is updated, with API keys read from environment variables.
* The turn footer's output speed and time to first token now count a step that only calls tools from when its tool call starts, on the Anthropic and Chat Completions protocols. Before, the whole step counted as output time, so a tool-using turn showed a speed far too low.
* In a multi-step turn the footer shows the cached tokens of the latest step, matching the input it shows, instead of adding up every step's cached tokens.
* Each model step in the diagnostic log (`HMJ_LOG`) records its time to first output (`ttft`) and cached tokens.
* Model requests no longer ask for gzip-compressed responses: an event stream gains little from compression, and a compressing proxy may hold the stream back before passing it on.
* The diagnostic log (`HMJ_LOG`) records each model response: how long its headers took, the HTTP version, any content encoding, and the provider's request ID.
* Anthropic, Gemini and OpenAI requests list map keys, including each tool's schema keys, in sorted order. They came out in a random order on each run, so the request bytes the API caches changed from run to run.

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
