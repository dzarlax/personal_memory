package rag

import (
	"strings"
	"unicode/utf8"
)

// chunk splits text into pieces no larger than maxBytes.
// For markdown files it splits on headings first, then paragraphs, then sentences.
func chunk(text string, maxBytes int, isMarkdown bool) []chunkResult {
	if isMarkdown {
		return chunkMarkdown(text, maxBytes)
	}
	return chunkPlain(text, maxBytes)
}

type chunkResult struct {
	text    string
	heading string
}

func chunkMarkdown(text string, maxBytes int) []chunkResult {
	sections := splitByHeadings(text)
	var results []chunkResult
	for _, sec := range sections {
		if len(sec.text) <= maxBytes {
			results = append(results, chunkResult{text: sec.text, heading: sec.heading})
			continue
		}
		// Split large sections at paragraph boundaries.
		paras := strings.Split(sec.text, "\n\n")
		var buf strings.Builder
		for _, para := range paras {
			para = strings.TrimSpace(para)
			if para == "" {
				continue
			}
			if buf.Len() > 0 && buf.Len()+len(para)+2 > maxBytes {
				results = append(results, chunkResult{text: buf.String(), heading: sec.heading})
				buf.Reset()
			}
			if len(para) > maxBytes {
				// Still too large — split at sentence boundaries.
				for _, piece := range splitSentences(para, maxBytes) {
					results = append(results, chunkResult{text: piece, heading: sec.heading})
				}
				continue
			}
			if buf.Len() > 0 {
				buf.WriteString("\n\n")
			}
			buf.WriteString(para)
		}
		if buf.Len() > 0 {
			results = append(results, chunkResult{text: buf.String(), heading: sec.heading})
		}
	}
	return results
}

type section struct {
	text    string
	heading string
}

func splitByHeadings(text string) []section {
	lines := strings.Split(text, "\n")
	var sections []section
	var currentHeading string
	var buf strings.Builder

	flush := func() {
		t := strings.TrimSpace(buf.String())
		if t != "" {
			sections = append(sections, section{text: t, heading: currentHeading})
		}
		buf.Reset()
	}

	for _, line := range lines {
		if isMarkdownHeading(line) {
			flush()
			if i := strings.Index(line, " "); i >= 0 {
				currentHeading = strings.TrimSpace(line[i+1:])
			}
			buf.WriteString(line + "\n")
		} else {
			buf.WriteString(line + "\n")
		}
	}
	flush()
	return sections
}

func splitSentences(text string, maxBytes int) []string {
	var results []string
	var buf strings.Builder
	for _, sentence := range strings.SplitAfter(text, ". ") {
		if buf.Len()+len(sentence) > maxBytes && buf.Len() > 0 {
			results = append(results, buf.String())
			buf.Reset()
		}
		// If a single sentence is still too large, hard-split at rune boundaries.
		if len(sentence) > maxBytes {
			for len(sentence) > 0 {
				n := maxBytes
				if n >= len(sentence) {
					n = len(sentence)
				} else {
					// Back up to the start of the current rune.
					for n > 1 && !utf8.RuneStart(sentence[n]) {
						n--
					}
				}
				results = append(results, sentence[:n])
				sentence = sentence[n:]
			}
			continue
		}
		buf.WriteString(sentence)
	}
	if buf.Len() > 0 {
		results = append(results, buf.String())
	}
	return results
}

func chunkPlain(text string, maxBytes int) []chunkResult {
	var results []chunkResult
	for _, piece := range splitSentences(text, maxBytes) {
		results = append(results, chunkResult{text: piece})
	}
	return results
}
