package docs

import (
	"fmt"
	"strings"
)

// chunkBody splits a markdown body into heading-bounded chunks. A '#' inside a
// fenced code block (``` or ~~~) never starts a chunk — a false split would
// corrupt section retrieval. Content before the first heading becomes a
// preamble chunk with an empty trail. #### and deeper are content, not
// boundaries, so deeply-nested docs do not shatter into useless fragments.
// Returns the first H1 (the doc's title when it has one), the flat #–###
// heading list (the UI outline + get_doc section lookup), and the chunks.
func chunkBody(path, group, body string) (title string, headings []string, chunks []Chunk) {
	var (
		trail    []string // heading ancestry of the open chunk
		levels   []int    // heading levels matching trail
		curText  []string
		curTrail []string
	)
	flush := func() {
		t := strings.TrimSpace(strings.Join(curText, "\n"))
		curText = nil
		if t == "" {
			return
		}
		chunks = append(chunks, Chunk{
			ID:    fmt.Sprintf("%s#%d", path, len(chunks)),
			Path:  path,
			Group: group,
			Trail: append([]string(nil), curTrail...),
			Text:  t,
		})
	}
	inFence := false
	for line := range strings.SplitSeq(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			curText = append(curText, line)
			continue
		}
		level := headingLevel(t)
		if inFence || level == 0 {
			curText = append(curText, line)
			continue
		}
		flush()
		h := strings.TrimSpace(strings.TrimLeft(t, "# "))
		// A new heading pops same-or-deeper siblings off the trail, then pushes.
		for len(levels) > 0 && levels[len(levels)-1] >= level {
			levels = levels[:len(levels)-1]
			trail = trail[:len(trail)-1]
		}
		levels = append(levels, level)
		trail = append(trail, h)
		if level == 1 && title == "" {
			title = h
		}
		headings = append(headings, h)
		curTrail = append([]string(nil), trail...)
	}
	flush()
	return title, headings, chunks
}

// headingLevel returns 1–3 for a #–### heading line, 0 otherwise. #### and
// deeper return 0 by design (see chunkBody).
func headingLevel(trimmed string) int {
	if !strings.HasPrefix(trimmed, "#") {
		return 0
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == '#' {
		n++
	}
	if n > 3 || n >= len(trimmed) || trimmed[n] != ' ' {
		return 0
	}
	return n
}
