package telegram

import "testing"

func TestMarkdownToHTML(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "hello world", "hello world"},
		{"bold double-star", "this is **bold** ok", "this is <b>bold</b> ok"},
		{"bold double-underscore", "this is __bold__ ok", "this is <b>bold</b> ok"},
		{"italic single-star", "really *cool* word", "really <i>cool</i> word"},
		{"italic underscore", "use _emphasis_ here", "use <i>emphasis</i> here"},
		{
			"identifier with underscores left alone",
			"variable snake_case_name in code",
			"variable snake_case_name in code",
		},
		{"strike", "this is ~~gone~~ now", "this is <s>gone</s> now"},
		{"inline code", "run `ls -la` first", "run <code>ls -la</code> first"},
		{
			"inline code escapes html",
			"run `cat <file>` now",
			"run <code>cat &lt;file&gt;</code> now",
		},
		{
			"fenced code block with language",
			"before\n```go\nfmt.Println(\"hi\")\n```\nafter",
			"before\n<pre><code class=\"language-go\">fmt.Println(&#34;hi&#34;)</code></pre>\nafter",
		},
		{
			"fenced code block without language",
			"```\nplain code\n```",
			"<pre>plain code</pre>",
		},
		{
			"link",
			"see [the docs](https://example.com)",
			"see <a href=\"https://example.com\">the docs</a>",
		},
		{
			"heading h2 to bold",
			"## Heading text\nbody",
			"<b>Heading text</b>\nbody",
		},
		{
			"bullet list",
			"items:\n- one\n- two\n- three",
			"items:\n• one\n• two\n• three",
		},
		{
			"escapes raw html in plain text",
			"compare a < b and c > d & done",
			"compare a &lt; b and c &gt; d &amp; done",
		},
		{
			"link url is escaped",
			"see [docs](https://x.com?a=1&b=2)",
			"see <a href=\"https://x.com?a=1&amp;b=2\">docs</a>",
		},
		{
			"bold inside paragraph with neighbours",
			"start **mid** end",
			"start <b>mid</b> end",
		},
		{
			"asterisks not greedy across newlines",
			"a *b\nc* d",
			"a *b\nc* d",
		},
		{
			"realistic weather reply",
			"**Wrocław dziś:**\n\nteraz: **15°C**, słonecznie",
			"<b>Wrocław dziś:</b>\n\nteraz: <b>15°C</b>, słonecznie",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := markdownToHTML(tc.in)
			if got != tc.want {
				t.Errorf("markdownToHTML(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}
