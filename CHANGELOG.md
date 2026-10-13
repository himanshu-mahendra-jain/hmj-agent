# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/2.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
