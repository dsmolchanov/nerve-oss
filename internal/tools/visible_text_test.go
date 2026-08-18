package tools

import "testing"

func TestVisibleTextRendersAsAReaderSeesIt(t *testing.T) {
	for name, testCase := range map[string]struct{ markup, want string }{
		"entity is decoded":          {"<p>guar&#97;ntee</p>", "guarantee"},
		"inline tag does not split":  {"<p>guar<b>a</b>ntee</p>", "guarantee"},
		"blocks keep a boundary":     {"<p>no</p><p>warranty</p>", "no warranty"},
		"break keeps a boundary":     {"one<br>two", "one two"},
		"style content is invisible": {"<style>.x{color:red}</style><p>hi</p>", "hi"},
		"script content is invisible": {
			"<script>var guarantee = 1;</script><p>hi</p>", "hi",
		},
		"attributes are not text":  {`<a href="https://example.test/guarantee">link</a>`, "link"},
		"unterminated tag is safe": {"<p>text<", "text"},
		"plain text is unchanged":  {"just text", "just text"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := visibleText(testCase.markup); got != testCase.want {
				t.Fatalf("expected %q, got %q", testCase.want, got)
			}
		})
	}
}
