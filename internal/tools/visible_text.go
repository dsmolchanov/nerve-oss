package tools

import (
	"html"
	"strings"
)

// blockLevelTags end a run of text the way a reader perceives it. Everything
// else is inline: a tag boundary inside a word does not separate the word.
var blockLevelTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"br": true, "dd": true, "div": true, "dl": true, "dt": true,
	"fieldset": true, "figcaption": true, "figure": true, "footer": true,
	"form": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true,
	"h6": true, "header": true, "hr": true, "li": true, "main": true,
	"nav": true, "ol": true, "p": true, "pre": true, "section": true,
	"table": true, "tbody": true, "td": true, "tfoot": true, "th": true,
	"thead": true, "tr": true, "ul": true,
}

// visibleText renders markup as a reader sees it, for content policy.
//
// policy.Evaluate matches literal substrings and regular expressions, so raw
// markup hides text from it in two ordinary ways: an entity (`guar&#97;ntee`)
// and an inline tag inside a word (`guar<b>a</b>ntee`). A model asked to
// produce HTML emits both without trying to.
//
// Inline boundaries therefore join with nothing, so a split word is restored,
// while block boundaries join with a space, so two adjacent paragraphs cannot
// fuse into a phrase neither of them contains. Script and style contents are
// dropped: they are not visible, so scanning them only invents violations.
func visibleText(markup string) string {
	var out strings.Builder
	var run strings.Builder
	skipUntilClose := ""

	flush := func(separator bool) {
		if run.Len() > 0 {
			out.WriteString(html.UnescapeString(run.String()))
			run.Reset()
		}
		if separator && out.Len() > 0 && !strings.HasSuffix(out.String(), " ") {
			out.WriteByte(' ')
		}
	}

	for index := 0; index < len(markup); index++ {
		if markup[index] != '<' {
			if skipUntilClose == "" {
				run.WriteByte(markup[index])
			}
			continue
		}
		closing := strings.IndexByte(markup[index:], '>')
		if closing < 0 {
			// Unterminated tag: the remainder is markup, not visible text.
			break
		}
		tag := markup[index+1 : index+closing]
		index += closing

		name := strings.ToLower(strings.TrimLeft(tag, "/!"))
		if cut := strings.IndexAny(name, " \t\r\n/"); cut >= 0 {
			name = name[:cut]
		}
		if skipUntilClose != "" {
			if strings.HasPrefix(tag, "/") && name == skipUntilClose {
				skipUntilClose = ""
			}
			continue
		}
		if name == "script" || name == "style" {
			flush(true)
			skipUntilClose = name
			continue
		}
		flush(blockLevelTags[name])
	}
	flush(false)

	return strings.TrimSpace(out.String())
}
