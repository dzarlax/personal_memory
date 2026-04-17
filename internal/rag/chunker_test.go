package rag

import (
	"strings"
	"testing"
)

func TestSplitByHeadings_RecognizesH1H2H3(t *testing.T) {
	text := `# Top

intro text

## Section A

body of A

### Subsection

sub body

## Section B

body of B
`
	secs := splitByHeadings(text)
	// Expect: [# Top + intro], [## Section A + body], [### Subsection + body], [## Section B + body]
	if len(secs) != 4 {
		t.Fatalf("expected 4 sections, got %d: %+v", len(secs), secs)
	}
	wantHeadings := []string{"Top", "Section A", "Subsection", "Section B"}
	for i, want := range wantHeadings {
		if secs[i].heading != want {
			t.Errorf("section %d: heading = %q, want %q", i, secs[i].heading, want)
		}
	}
}

func TestSplitByHeadings_IgnoresNonHeadingHashLines(t *testing.T) {
	// Shebangs, hashtag-style lines, and code should NOT become sections.
	text := `# Title

#!/bin/bash
#include <stdio.h>
#todo buy milk

body
`
	secs := splitByHeadings(text)
	if len(secs) != 1 {
		t.Fatalf("expected 1 section, got %d: %+v", len(secs), secs)
	}
	if secs[0].heading != "Title" {
		t.Errorf("heading = %q, want %q", secs[0].heading, "Title")
	}
}

func TestSplitByHeadings_HeadingTextPreserved(t *testing.T) {
	// Regression: strings.TrimLeft(line, "#") would strip # in the middle too.
	text := `## Topic #1: intro

body
`
	secs := splitByHeadings(text)
	if len(secs) != 1 {
		t.Fatalf("expected 1 section, got %d", len(secs))
	}
	if secs[0].heading != "Topic #1: intro" {
		t.Errorf("heading = %q, want %q (inner # must be preserved)", secs[0].heading, "Topic #1: intro")
	}
}

func TestSplitSentences_HardSplitTerminates(t *testing.T) {
	// Regression: the hard-split loop could infinite-loop with small maxBytes.
	// Build a sentence (no ". ") longer than maxBytes with multi-byte runes.
	text := strings.Repeat("привет", 200) // cyrillic, 2 bytes each rune
	pieces := splitSentences(text, 100)
	if len(pieces) == 0 {
		t.Fatal("expected non-empty pieces")
	}
	// Every piece must be within limits and non-empty.
	for i, p := range pieces {
		if p == "" {
			t.Errorf("piece %d is empty", i)
		}
		if len(p) > 100 {
			t.Errorf("piece %d exceeds maxBytes: %d", i, len(p))
		}
	}
	// Concatenation must equal input.
	if got := strings.Join(pieces, ""); got != text {
		t.Errorf("concatenation mismatch")
	}
}

func TestChunk_Markdown_SmallFileSingleChunk(t *testing.T) {
	text := "# Title\n\nhello world\n"
	got := chunk(text, 1500, true)
	if len(got) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(got))
	}
	if got[0].heading != "Title" {
		t.Errorf("heading = %q, want Title", got[0].heading)
	}
}

func TestChunk_Plain_NoHeadings(t *testing.T) {
	text := "line one. line two. line three."
	got := chunk(text, 1500, false)
	if len(got) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(got))
	}
	if got[0].heading != "" {
		t.Errorf("plain chunk should have empty heading, got %q", got[0].heading)
	}
}

func TestIsMarkdownHeading(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"# H1", true},
		{"## H2", true},
		{"### H3", true},
		{"#### H4", false}, // we only split on H1-H3
		{"#notag", false},
		{"#!/bin/bash", false},
		{"not a heading", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isMarkdownHeading(c.line); got != c.want {
			t.Errorf("isMarkdownHeading(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}
