package main

import (
	"strings"
	"testing"
)

const docspackFixture = `<html><head><title>T</title></head><body>
<nav class="sidebar"><p>Navigation link farm that should never survive cleaning at all</p></nav>
<main>
<h1>Widget Guide</h1>
<p>Widgets are configured through a single YAML file at startup time.</p>
<h2>Install Steps</h2>
<ul><li>Download the widget binary for your platform today</li>
<li>Run the widget binary with the serve command flag</li></ul>
<pre>widget serve --port 8080 --config widget.yaml</pre>
</main>
<footer><p>Copyright footer boilerplate that should be dropped entirely</p></footer>
</body></html>`

func TestConvertWithDocspack(t *testing.T) {
	md := convertWithDocspack([]byte(docspackFixture))
	for _, want := range []string{"# Widget Guide", "## Install Steps", "widget serve --port 8080", "```", "- Download the widget binary"} {
		if !strings.Contains(md, want) {
			t.Errorf("docspack output missing %q\n---\n%s", want, md)
		}
	}
	for _, banned := range []string{"link farm", "footer boilerplate"} {
		if strings.Contains(md, banned) {
			t.Errorf("docspack output leaked boilerplate %q\n---\n%s", banned, md)
		}
	}
}

func TestDocspackEnabledDefaultOff(t *testing.T) {
	t.Setenv("DOCS_PULLER_DOCSPACK", "")
	if docspackEnabled() {
		t.Fatal("docspack path must be OFF by default")
	}
	t.Setenv("DOCS_PULLER_DOCSPACK", "1")
	if !docspackEnabled() {
		t.Fatal("DOCS_PULLER_DOCSPACK=1 must enable the docspack path")
	}
	t.Setenv("DOCS_PULLER_DOCSPACK", "true")
	if docspackEnabled() {
		t.Fatal("only the exact value 1 enables the docspack path")
	}
}
