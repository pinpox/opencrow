package telegram

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// markdownToHTML converts a subset of CommonMark markdown to the HTML
// dialect Telegram's parse_mode=HTML accepts.
//
// Telegram supports only: b, i, u, s, span class="tg-spoiler", tg-spoiler,
// a href, code, pre, blockquote. There are no headings, lists, or tables —
// we approximate (`#` headings → bold lines, `-`/`*` bullets → "• ", numbered
// lists pass through). HTML special characters in user content are escaped.
//
// The output is best-effort: callers should fall back to sending the raw
// text without parse_mode if Telegram rejects the rendered HTML (e.g. an
// unbalanced asterisk slipping past the regex into a stray `<i>`).
func markdownToHTML(s string) string {
	if s == "" {
		return ""
	}

	// Stash code blocks and links first so their contents don't get
	// chewed up by the bold/italic/escape passes. Each placeholder is a
	// short token containing only NUL bytes and digits — html.EscapeString
	// leaves it alone, and no regex below matches it.
	var stash []string

	store := func(rendered string) string {
		idx := len(stash)
		stash = append(stash, rendered)

		return fmt.Sprintf("\x00P%d\x00", idx)
	}

	// 1. Fenced code blocks.
	s = codeBlockRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := codeBlockRe.FindStringSubmatch(m)
		lang := strings.TrimSpace(sub[1])
		body := html.EscapeString(strings.TrimRight(sub[2], "\n"))

		if lang != "" {
			return store(`<pre><code class="language-` + html.EscapeString(lang) + `">` + body + `</code></pre>`)
		}

		return store("<pre>" + body + "</pre>")
	})

	// 2. Inline code (single backticks).
	s = inlineCodeRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := inlineCodeRe.FindStringSubmatch(m)

		return store("<code>" + html.EscapeString(sub[1]) + "</code>")
	})

	// 3. Links [text](url).
	s = linkRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := linkRe.FindStringSubmatch(m)

		return store(`<a href="` + html.EscapeString(sub[2]) + `">` + html.EscapeString(sub[1]) + "</a>")
	})

	// 4. Escape HTML special chars in everything that's left.
	s = html.EscapeString(s)

	// 5. Block-level: ATX headings → bold line. Skip the heading marker
	// and bold the text. Drop trailing "###" closers if present.
	s = headingRe.ReplaceAllString(s, "<b>$2</b>")

	// 6. Bullets — "- ", "* ", "+ " at line start → "• ".
	s = bulletRe.ReplaceAllString(s, "$1• ")

	// 7. Inline emphasis. **bold** before *italic* so the second pass
	// only sees single asterisks.
	s = boldDoubleRe.ReplaceAllString(s, "<b>$1</b>")
	s = boldUnderRe.ReplaceAllString(s, "<b>$1</b>")
	s = strikeRe.ReplaceAllString(s, "<s>$1</s>")
	s = italicAstRe.ReplaceAllString(s, "$1<i>$2</i>$3")
	s = italicUnderRe.ReplaceAllString(s, "$1<i>$2</i>$3")

	// 8. Restore placeholders.
	for i, val := range stash {
		s = strings.Replace(s, fmt.Sprintf("\x00P%d\x00", i), val, 1)
	}

	return s
}

var (
	codeBlockRe  = regexp.MustCompile("(?s)```(\\w*)\\n?(.*?)```")
	inlineCodeRe = regexp.MustCompile("`([^`\n]+)`")
	linkRe       = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\s]+)\)`)
	headingRe    = regexp.MustCompile(`(?m)^(#{1,6})\s+(.+?)\s*#*\s*$`)
	bulletRe     = regexp.MustCompile(`(?m)^([ \t]*)[-*+]\s+`)
	boldDoubleRe = regexp.MustCompile(`\*\*([^\*\n]+?)\*\*`)
	boldUnderRe  = regexp.MustCompile(`__([^_\n]+?)__`)
	strikeRe     = regexp.MustCompile(`~~([^~\n]+?)~~`)
	// Italic with `*foo*` — require the asterisks to NOT be adjacent to
	// another asterisk (so `**bold**` left alone after the bold pass
	// has no leftovers anyway, but be defensive). $1/$3 are the
	// surrounding chars (or empty at line bounds).
	italicAstRe = regexp.MustCompile(`(^|[^\*\w])\*([^\*\n]+?)\*([^\*\w]|$)`)
	// Italic with `_foo_` — require word-boundary on both sides so we
	// don't munge identifiers like `snake_case_var`.
	italicUnderRe = regexp.MustCompile(`(^|\W)_([^_\n]+?)_(\W|$)`)
)
