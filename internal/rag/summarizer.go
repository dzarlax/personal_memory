package rag

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// folderSummary builds a text summary of a directory for embedding — no LLM needed.
func folderSummary(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	var topics []string
	seenTopics := map[string]bool{}
	var snippets []string
	textFileCount := 0

	for _, fname := range files {
		ext := strings.ToLower(filepath.Ext(fname))
		if ext != ".md" && ext != ".txt" && ext != ".markdown" {
			continue
		}
		fpath := filepath.Join(dir, fname)
		heading, snippet := firstHeadingAndSnippet(fpath)
		if heading != "" && !seenTopics[heading] && len(topics) < 20 {
			seenTopics[heading] = true
			topics = append(topics, heading)
		}
		if snippet != "" && textFileCount < 5 {
			snippets = append(snippets, snippet)
		}
		textFileCount++
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Folder: %s\n", dir))
	sb.WriteString(fmt.Sprintf("Files (%d): %s\n", len(files), strings.Join(files, ", ")))
	if len(topics) > 0 {
		sb.WriteString(fmt.Sprintf("Topics: %s\n", strings.Join(topics, ", ")))
	}
	if len(snippets) > 0 {
		sb.WriteString("Snippets:\n")
		for _, s := range snippets {
			sb.WriteString("  - " + s + "\n")
		}
	}
	return sb.String(), nil
}

// firstHeadingAndSnippet reads a file and extracts the first H1/H2 heading and first body line.
func firstHeadingAndSnippet(path string) (heading, snippet string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if heading == "" {
				if i := strings.Index(line, " "); i >= 0 {
					heading = strings.TrimSpace(line[i+1:])
				}
			}
			continue
		}
		if snippet == "" {
			if len(line) > 120 {
				line = line[:120] + "…"
			}
			snippet = line
		}
		if heading != "" && snippet != "" {
			break
		}
	}
	return heading, snippet
}
