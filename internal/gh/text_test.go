package gh

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// badgeWall renders enough badge markup to outweigh any byte budget the
// caller would set: the shape of a README that opens with a screen of
// shields, a centred header and a table of contents.
func badgeWall(bytes int) string {
	var b strings.Builder
	b.WriteString("<p align=\"center\">\n  <img src=\"https://raw.githubusercontent.com/acme/orbit/main/docs/logo.png\" width=\"200\" alt=\"logo\">\n</p>\n\n")
	b.WriteString("<h1 align=\"center\">Orbit</h1>\n\n<p align=\"center\">\n")
	for b.Len() < bytes {
		b.WriteString("  <a href=\"https://github.com/acme/orbit/actions\"><img src=\"https://img.shields.io/github/actions/workflow/status/acme/orbit/ci.yml?branch=main&style=flat-square\" alt=\"build\"></a>\n")
		b.WriteString("[![Coverage](https://img.shields.io/codecov/c/github/acme/orbit?style=flat-square)](https://codecov.io/gh/acme/orbit)\n")
		b.WriteString("[![Go Report Card](https://goreportcard.com/badge/github.com/acme/orbit)](https://goreportcard.com/report/github.com/acme/orbit)\n")
	}
	b.WriteString("</p>\n\n")
	b.WriteString("## Table of Contents\n\n- [Features](#features)\n- [Installation](#installation)\n- [Usage](#usage)\n- [License](#license)\n\n")
	b.WriteString("- [Overview](#overview) · [Docs](https://orbit.example.com)\n\n")
	return b.String()
}

const orbitSummary = "Orbit is a lightweight job scheduler for Kubernetes that runs batch workloads on spare capacity, " +
	"preempting them when the cluster needs the room and resuming them from a checkpoint when it frees up again."

// Each fixture is written the way READMEs actually are, and the checks are
// on what the tokenizer would see: the summary must be there, and the parts
// that are the same in every project must not.
func TestReadmeOpening(t *testing.T) {
	for _, tc := range []struct {
		name     string
		markdown string
		maxWords int
		want     []string // substrings that must appear, in this order
		wantNot  []string // substrings that must not appear anywhere
	}{
		{
			// The first 8 KB is badges, an HTML header and a table of
			// contents; the old byte cut never reached the prose.
			name:     "badges before the prose",
			markdown: badgeWall(8192) + "## Overview\n\n" + orbitSummary + "\n\n## Installation\n\n```sh\ngo install github.com/acme/orbit@latest\n```\n",
			maxWords: 120,
			want:     []string{"Orbit is a lightweight job scheduler", "resuming them from a checkpoint"},
			wantNot:  []string{"https", "img", "shields", "codecov", "go install", "Features Installation"},
		},
		{
			name: "title, tagline, summary, then installation",
			markdown: `# Orbit

> Batch scheduling on spare capacity.

[![CI](https://img.shields.io/badge/ci-passing-green)](https://ci.example.com)

` + orbitSummary + `

## Installation

Requires Go 1.22 or later.

` + "```" + `
npm install -g orbit
` + "```" + `

## License

MIT
`,
			maxWords: 120,
			want:     []string{"Orbit", "Batch scheduling on spare capacity", "Orbit is a lightweight job scheduler"},
			wantNot:  []string{"npm install", "Requires Go", "MIT", "shields"},
		},
		{
			name:     "no headings at all",
			markdown: "A tiny library.\n\n" + orbitSummary + "\n\nIt is used in production at several companies.\n",
			maxWords: 120,
			want:     []string{"A tiny library", "Orbit is a lightweight job scheduler", "used in production"},
		},
		{
			// A one-line summary followed by the real description: both
			// are wanted, and the cut counts them together.
			name: "short summary then a longer paragraph",
			markdown: `# Orbit

Schedules batch jobs on spare capacity.

` + orbitSummary + `

## Usage

Run ` + "`orbit up`" + ` and point it at your cluster.
`,
			maxWords: 120,
			want:     []string{"Orbit", "Schedules batch jobs on spare capacity", "Orbit is a lightweight job scheduler"},
			wantNot:  []string{"orbit up", "point it at your cluster"},
		},
		{
			// Every section is one the extractor skips; there is still a
			// title, and it is better than nothing.
			name: "every section skipped",
			markdown: `# Orbit

## Installation

go install github.com/acme/orbit@latest

## Usage

orbit up

## License

Apache 2.0
`,
			maxWords: 120,
			want:     []string{"Orbit"},
			wantNot:  []string{"go install", "orbit up", "Apache"},
		},
		{
			name:     "cut inside multibyte text",
			markdown: "# 軌道\n\n軌道は Kubernetes の空き容量でバッチジョブを実行する軽量なスケジューラです 🚀 preempting jobs when the cluster needs the room and resuming them later from a checkpoint 😀 with no data loss at all.\n",
			maxWords: 12,
			want:     []string{"軌道", "🚀"},
		},
		{
			// Setext headings: an underlined title, and an underlined
			// "Installation" that must still be recognised as one.
			name: "setext headings",
			markdown: `Orbit
=====

` + orbitSummary + `

Installation
------------

Download the binary for your platform from the releases page and put it on your PATH.
`,
			maxWords: 120,
			want:     []string{"Orbit", "Orbit is a lightweight job scheduler"},
			wantNot:  []string{"Download the binary", "=", "-"},
		},
		{
			// The summary sits under an H2 after a skipped one, and a
			// reference-style link definition and a table are in the way.
			name: "summary after a skipped section, tables and link definitions",
			markdown: `# Orbit

[docs]: https://orbit.example.com
[ci]: https://ci.example.com/acme/orbit

## Requirements

| Component  | Version |
|------------|---------|
| Kubernetes | 1.28+   |

## About

` + orbitSummary + `

See the [docs][docs] for more.
`,
			maxWords: 120,
			want:     []string{"Orbit", "Orbit is a lightweight job scheduler", "See the docs for more"},
			wantNot:  []string{"Component", "1.28", "https", "|"},
		},
	} {
		got := readmeOpening(tc.markdown, tc.maxWords)
		if got == "" {
			t.Errorf("%s: nothing extracted", tc.name)
			continue
		}
		if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("%s: invalid UTF-8 in %q", tc.name, got)
		}
		if n := wordCount(got); tc.maxWords > 0 && n > tc.maxWords {
			t.Errorf("%s: %d words, want at most %d: %q", tc.name, n, tc.maxWords, got)
		}
		if strings.Contains(got, "  ") || got != strings.TrimSpace(got) {
			t.Errorf("%s: whitespace not collapsed: %q", tc.name, got)
		}
		rest := got
		for _, w := range tc.want {
			i := strings.Index(rest, w)
			if i < 0 {
				t.Errorf("%s: %q missing or out of order in %q", tc.name, w, got)
				break
			}
			rest = rest[i+len(w):]
		}
		for _, w := range tc.wantNot {
			if strings.Contains(got, w) {
				t.Errorf("%s: %q should have been stripped from %q", tc.name, w, got)
			}
		}
	}
}

// The word budget is spent on what describes the project, and the sections
// every README has do not count against it.
func TestReadmeOpeningBudget(t *testing.T) {
	markdown := "# Orbit\n\n" + orbitSummary + "\n\nA second paragraph that goes on to describe the scheduling model in enough detail to matter.\n\n## Installation\n\nnot this\n\n## Features\n\n- Checkpoints\n- Preemption\n"
	got := readmeOpening(markdown, 0)
	for _, w := range []string{"Orbit ", "resuming them from a checkpoint", "A second paragraph", "Checkpoints Preemption"} {
		if !strings.Contains(got, w) {
			t.Errorf("with no budget, %q is missing from %q", w, got)
		}
	}
	if strings.Contains(got, "not this") {
		t.Errorf("a skipped section leaked into %q", got)
	}
	if got := readmeOpening(markdown, 5); wordCount(got) != 5 {
		t.Errorf("a budget of 5 words kept %d: %q", wordCount(got), got)
	}
	if got := readmeOpening("", 120); got != "" {
		t.Errorf("an empty README produced %q", got)
	}
	if got := readmeOpening("[![a](https://x/y.svg)](https://x)\n\n<!-- nothing -->\n", 120); got != "" {
		t.Errorf("a README of nothing but badges produced %q", got)
	}
}

func TestSkipHeading(t *testing.T) {
	for title, want := range map[string]bool{
		"Installation":               true,
		"🚀 Quick start":              true,
		"Getting Started with Orbit": true,
		"License (MIT)":              true,
		"3. Building from source":    true,
		"Table of contents":          true,
		"Supported platforms":        false, // "support" is a word, "supported" is not
		"Features":                   false,
		"Why Orbit?":                 false,
		"":                           false,
	} {
		if got := skipHeading(title); got != want {
			t.Errorf("skipHeading(%q) = %v, want %v", title, got, want)
		}
	}
}

// The byte cap must never split a rune: a partial sequence would reach the
// tokenizer as U+FFFD and end up in the vocabulary.
func TestCapBytes(t *testing.T) {
	cjk := strings.Repeat("軌道は軽量なスケジューラです", 20) // one word, no spaces
	for limit := 1; limit < 64; limit++ {
		got := capBytes(cjk, limit)
		if len(got) > limit {
			t.Fatalf("limit %d: kept %d bytes", limit, len(got))
		}
		if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
			t.Fatalf("limit %d: invalid UTF-8 in %q", limit, got)
		}
	}
	if got := capBytes("alpha beta gamma delta", 12); got != "alpha beta" {
		t.Errorf("the cut should land on a word boundary, got %q", got)
	}
	if got := capBytes("short", 0); got != "short" {
		t.Errorf("a limit of 0 should keep everything, got %q", got)
	}
	if got := capBytes("short", 100); got != "short" {
		t.Errorf("a limit above the length should keep everything, got %q", got)
	}
}
