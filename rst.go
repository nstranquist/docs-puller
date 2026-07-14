package main

import (
	"regexp"
	"strings"
)

var (
	rstDocRoleRE     = regexp.MustCompile(`(?i):(?:doc|ref):\x60([^\x60]+)\x60`)
	rstTextRoleRE    = regexp.MustCompile(`(?i):(?:menuselection|kbd|guilabel|term|abbr|class|func|meth|mod|attr|data):\x60([^\x60]+)\x60`)
	rstGenericRoleRE = regexp.MustCompile(`(?i):[a-z][a-z0-9_-]*:\x60([^\x60]+)\x60`)
	rstRoleOpenRE    = regexp.MustCompile(`(?i):[a-z][a-z0-9_-]*:\x60`)
	rstLinkRE        = regexp.MustCompile("`([^`<>]+?)\\s*<([^>]+)>`_+")
	rstInlineImageRE = regexp.MustCompile(`(?i)\.\.\s+(?:figure|image)::\s+(\S+)`)
	rstInlineDirRE   = regexp.MustCompile(`(?i)\.\.\s+(rubric|list-table|note|tip|warning|important|caution|seealso|toctree|include|literalinclude|only)::`)
)

// rstToMarkdown preserves Sphinx/reStructuredText source as agent-readable
// Markdown without needing a Python/docutils runtime. It intentionally focuses
// on the structures that affect retrieval quality: headings, canonical links,
// code blocks, figures, admonitions, and inline roles. Unknown directives stay
// visible as plain text rather than being silently discarded.
func rstToMarkdown(input []byte) []byte {
	lines := rstJoinWrappedRoles(strings.Split(strings.ReplaceAll(string(input), "\r\n", "\n"), "\n"))
	levels := map[string]int{}
	nextLevel := 1
	var out []string

	levelFor := func(ch byte, overline bool) int {
		key := string([]byte{ch})
		if overline {
			key += ":over"
		}
		if level, ok := levels[key]; ok {
			return level
		}
		level := nextLevel
		if level > 6 {
			level = 6
		}
		levels[key] = level
		nextLevel++
		return level
	}

	for i := 0; i < len(lines); {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if i+2 < len(lines) && rstAdornment(trimmed) && strings.TrimSpace(lines[i+1]) != "" && strings.TrimSpace(lines[i+2]) == trimmed {
			level := levelFor(trimmed[0], true)
			out = append(out, strings.Repeat("#", level)+" "+rstInline(strings.TrimSpace(lines[i+1])), "")
			i += 3
			continue
		}
		if trimmed != "" && i+1 < len(lines) && rstAdornment(strings.TrimSpace(lines[i+1])) {
			adornment := strings.TrimSpace(lines[i+1])
			level := levelFor(adornment[0], false)
			out = append(out, strings.Repeat("#", level)+" "+rstInline(trimmed), "")
			i += 2
			continue
		}

		if strings.HasPrefix(trimmed, ".. _") && strings.HasSuffix(trimmed, ":") {
			i++
			continue
		}
		if strings.HasPrefix(trimmed, ".. index::") {
			i++
			continue
		}
		if kind, arg, ok := rstDirective(trimmed); ok {
			switch kind {
			case "rubric":
				out = append(out, "### "+rstInline(arg), "")
				i++
				continue
			case "include", "literalinclude":
				label := "Included source"
				if kind == "literalinclude" {
					label = "Included code"
				}
				out = append(out, "> "+label+": ["+arg+"]("+arg+")", "")
				_, i = rstDirectiveBody(lines, i)
				continue
			case "toctree":
				body, next := rstDirectiveBody(lines, i)
				out = append(out, rstToctreeMarkdown(body)...)
				i = next
				continue
			case "list-table":
				body, next := rstDirectiveBody(lines, i)
				out = append(out, rstListTableMarkdown(arg, body)...)
				i = next
				continue
			case "only":
				body, next := rstDirectiveBody(lines, i)
				if arg != "" {
					out = append(out, "_Applies when: "+rstInline(arg)+"._", "")
				}
				for _, bodyLine := range body {
					out = append(out, rstInline(bodyLine))
				}
				out = append(out, "")
				i = next
				continue
			case "figure", "image":
				label := strings.ToUpper(kind[:1]) + kind[1:]
				body, next := rstDirectiveBody(lines, i)
				alt := label
				var caption []string
				for _, bodyLine := range body {
					trimmedBody := strings.TrimSpace(bodyLine)
					if strings.HasPrefix(strings.ToLower(trimmedBody), ":alt:") {
						alt = strings.TrimSpace(trimmedBody[len(":alt:"):])
						continue
					}
					if strings.HasPrefix(trimmedBody, ":") {
						continue
					}
					if trimmedBody != "" {
						caption = append(caption, rstInline(trimmedBody))
					}
				}
				out = append(out, "!["+alt+"]("+arg+")")
				out = append(out, caption...)
				out = append(out, "")
				i = next
				continue
			case "code-block", "code":
				body, next := rstDirectiveBody(lines, i)
				out = append(out, "```"+arg)
				started := false
				for _, bodyLine := range body {
					trimmedBody := strings.TrimSpace(bodyLine)
					if !started && (trimmedBody == "" || strings.HasPrefix(trimmedBody, ":")) {
						continue
					}
					started = true
					if trimmedBody == "" {
						out = append(out, "")
					} else {
						out = append(out, bodyLine)
					}
				}
				for len(out) > 0 && out[len(out)-1] == "" {
					out = out[:len(out)-1]
				}
				out = append(out, "```", "")
				i = next
				continue
			case "note", "tip", "warning", "important", "caution", "seealso":
				label := strings.ToUpper(kind)
				if kind == "seealso" {
					label = "SEE ALSO"
				}
				out = append(out, "> [!"+label+"]")
				if arg != "" {
					out = append(out, "> "+rstInline(arg))
				}
				i++
				for i < len(lines) {
					if strings.TrimSpace(lines[i]) == "" {
						out = append(out, ">")
						i++
						continue
					}
					if !strings.HasPrefix(lines[i], "   ") && !strings.HasPrefix(lines[i], "\t") {
						break
					}
					out = append(out, "> "+rstInline(strings.TrimPrefix(strings.TrimPrefix(lines[i], "   "), "\t")))
					i++
				}
				out = append(out, "")
				continue
			}
		}

		out = append(out, rstInline(line))
		i++
	}

	return []byte(strings.TrimSpace(strings.Join(out, "\n")) + "\n")
}

func rstAdornment(s string) bool {
	if len(s) < 3 {
		return false
	}
	allowed := "=-~^\"'`:.+*#"
	if !strings.ContainsRune(allowed, rune(s[0])) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return true
}

func rstDirective(line string) (kind, arg string, ok bool) {
	if !strings.HasPrefix(line, ".. ") {
		return "", "", false
	}
	body := strings.TrimPrefix(line, ".. ")
	kind, arg, ok = strings.Cut(body, "::")
	if !ok {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(kind)), strings.TrimSpace(arg), true
}

// rstDirectiveBody consumes the indented block following a directive and
// removes one RST indentation level. Blank separator lines are retained so
// paragraphs and list structure remain readable after conversion.
func rstDirectiveBody(lines []string, start int) ([]string, int) {
	var body []string
	i := start + 1
	for i < len(lines) {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			body = append(body, "")
			i++
			continue
		}
		if !strings.HasPrefix(line, "   ") && !strings.HasPrefix(line, "\t") {
			break
		}
		body = append(body, strings.TrimPrefix(strings.TrimPrefix(line, "   "), "\t"))
		i++
	}
	return body, i
}

func rstToctreeMarkdown(body []string) []string {
	var out []string
	for _, line := range body {
		entry := strings.TrimSpace(line)
		if entry == "" || strings.HasPrefix(entry, ":") {
			continue
		}
		label, target := entry, entry
		if before, after, ok := strings.Cut(entry, "<"); ok && strings.HasSuffix(strings.TrimSpace(after), ">") {
			label = strings.TrimSpace(before)
			target = strings.TrimSuffix(strings.TrimSpace(after), ">")
		} else {
			label = strings.Trim(strings.TrimPrefix(target, "/"), "` ")
			if idx := strings.LastIndex(label, "/"); idx >= 0 {
				label = label[idx+1:]
			}
			if ext := filepathExtension(label); strings.EqualFold(ext, ".rst") {
				label = label[:len(label)-len(ext)]
			}
			label = strings.ReplaceAll(label, "_", " ")
		}
		if target != "self" && filepathExtension(target) == "" && !strings.ContainsAny(target, "*#?") {
			target += ".md"
		} else if strings.EqualFold(filepathExtension(target), ".rst") {
			target = target[:len(target)-4] + ".md"
		}
		out = append(out, "- ["+rstInline(label)+"]("+target+")")
	}
	if len(out) > 0 {
		out = append(out, "")
	}
	return out
}

func filepathExtension(path string) string {
	lastSlash := strings.LastIndex(path, "/")
	lastDot := strings.LastIndex(path, ".")
	if lastDot <= lastSlash {
		return ""
	}
	return path[lastDot:]
}

func rstListTableMarkdown(title string, body []string) []string {
	var out []string
	if title != "" {
		out = append(out, "### "+rstInline(title), "")
	}
	var row []string
	flush := func() {
		if len(row) > 0 {
			out = append(out, "- "+strings.Join(row, " | "))
			row = nil
		}
	}
	for _, line := range body {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ":") {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "* -"):
			flush()
			row = append(row, rstInline(strings.TrimSpace(strings.TrimPrefix(trimmed, "* -"))))
		case strings.HasPrefix(trimmed, "-"):
			row = append(row, rstInline(strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))))
		case len(row) > 0:
			row[len(row)-1] = strings.TrimSpace(row[len(row)-1] + " " + rstInline(trimmed))
		default:
			out = append(out, rstInline(trimmed))
		}
	}
	flush()
	if len(out) > 0 {
		out = append(out, "")
	}
	return out
}

func rstJoinWrappedRoles(lines []string) []string {
	joined := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		for rstHasUnclosedRole(line) && i+1 < len(lines) {
			i++
			line = strings.TrimRight(line, " \t") + " " + strings.TrimSpace(lines[i])
		}
		joined = append(joined, line)
	}
	return joined
}

func rstHasUnclosedRole(line string) bool {
	for _, open := range rstRoleOpenRE.FindAllStringIndex(line, -1) {
		if !strings.Contains(line[open[1]:], "`") {
			return true
		}
	}
	return false
}

func rstInline(line string) string {
	// Directives nested inside list-table/admonition bodies no longer begin a
	// source line after their parent block is flattened. Normalize their
	// inline form too; fenced RST examples bypass rstInline and stay literal.
	line = rstInlineImageRE.ReplaceAllString(line, "![Figure]($1)")
	line = rstInlineDirRE.ReplaceAllStringFunc(line, func(match string) string {
		parts := rstInlineDirRE.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		label := strings.ReplaceAll(strings.ToUpper(parts[1][:1])+strings.ToLower(parts[1][1:]), "-", " ")
		if strings.EqualFold(parts[1], "list-table") {
			label = "Table"
		} else if strings.EqualFold(parts[1], "seealso") {
			label = "See also"
		}
		return label + ":"
	})
	line = rstLinkRE.ReplaceAllString(line, "[$1]($2)")
	line = rstDocRoleRE.ReplaceAllStringFunc(line, func(match string) string {
		parts := rstDocRoleRE.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		label, target := parts[1], parts[1]
		if before, after, ok := strings.Cut(parts[1], "<"); ok {
			label = strings.TrimSpace(before)
			target = strings.TrimSuffix(strings.TrimSpace(after), ">")
		}
		if label == target {
			label = strings.Trim(strings.TrimPrefix(target, "/"), "`")
			if i := strings.LastIndex(label, "/"); i >= 0 {
				label = label[i+1:]
			}
			label = strings.ReplaceAll(label, "_", " ")
		}
		return "[" + label + "](" + target + ")"
	})
	line = rstTextRoleRE.ReplaceAllString(line, "`$1`")
	line = rstGenericRoleRE.ReplaceAllString(line, "`$1`")
	line = strings.ReplaceAll(line, "``", "`")
	return line
}
