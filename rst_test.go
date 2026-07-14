package main

import (
	"strings"
	"testing"
)

func TestRSTToMarkdownPreservesRetrievalStructure(t *testing.T) {
	input := []byte("************\n" +
		"Introduction\n" +
		"************\n\n" +
		"Geometry Nodes modifies objects with a :doc:`Geometry Nodes Modifier </modeling/modifiers/geometry_nodes>`.\n\n" +
		"GPU Rendering\n" +
		"=============\n\n" +
		"Use :menuselection:`Preferences --> System`.\n\n" +
		".. note::\n\n" +
		"   This is important.\n\n" +
		".. code-block:: python\n\n" +
		"   bpy.context.scene.render.engine = \"BLENDER_EEVEE\"\n")
	got := string(rstToMarkdown(input))
	for _, want := range []string{
		"# Introduction",
		"## GPU Rendering",
		"[Geometry Nodes Modifier](/modeling/modifiers/geometry_nodes)",
		"`Preferences --> System`",
		"> [!NOTE]",
		"```python",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("converted RST missing %q:\n%s", want, got)
		}
	}
}

func TestRSTLocalHelpers(t *testing.T) {
	if got := gitRepoSourceName("https://projects.blender.org/blender/blender-manual.git"); got != "blender-manual" {
		t.Fatalf("gitRepoSourceName = %q", got)
	}
	if got := canonicalLocalOriginURL("https://docs.blender.org/manual/en/latest/", "modeling/geometry_nodes/index.rst"); got != "https://docs.blender.org/manual/en/latest/modeling/geometry_nodes/index.html" {
		t.Fatalf("canonicalLocalOriginURL = %q", got)
	}
	if got := markdownOutputRel("modeling/INTRODUCTION.RST"); got != "modeling/INTRODUCTION.md" {
		t.Fatalf("markdownOutputRel = %q", got)
	}
}

func TestRSTToMarkdownConvertsRolesInHeadingsAndWrappedRoles(t *testing.T) {
	input := []byte(":Doc:`Geometry Nodes </modeling/geometry_nodes/index>`\n" +
		"==============================================================\n\n" +
		"See :DOC:`the modifier\n" +
		"</modeling/modifiers/geometry_nodes>` and :custom:`socket value`.\n")
	got := string(rstToMarkdown(input))
	for _, want := range []string{
		"# [Geometry Nodes](/modeling/geometry_nodes/index)",
		"[the modifier](/modeling/modifiers/geometry_nodes)",
		"`socket value`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("converted RST missing %q:\n%s", want, got)
		}
	}
	for _, raw := range []string{":Doc:`", ":DOC:`", ":custom:`"} {
		if strings.Contains(got, raw) {
			t.Errorf("converted RST retained role %q:\n%s", raw, got)
		}
	}
}

func TestRSTToMarkdownConvertsCommonSphinxDirectives(t *testing.T) {
	input := []byte(".. rubric:: Related Pages\n\n" +
		".. toctree::\n" +
		"   :maxdepth: 1\n\n" +
		"   Overview <overview>\n" +
		"   setup/install.rst\n\n" +
		".. include:: shared-note.rst\n\n" +
		".. list-table:: Render Engines\n" +
		"   :header-rows: 1\n\n" +
		"   * - Engine\n" +
		"     - Use\n" +
		"   * - Cycles\n" +
		"     - Path tracing\n\n" +
		".. only:: html\n\n" +
		"   Visible in the HTML manual.\n")
	got := string(rstToMarkdown(input))
	for _, want := range []string{
		"### Related Pages",
		"- [Overview](overview.md)",
		"- [install](setup/install.md)",
		"> Included source: [shared-note.rst](shared-note.rst)",
		"### Render Engines",
		"- Engine | Use",
		"- Cycles | Path tracing",
		"_Applies when: html._",
		"Visible in the HTML manual.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("converted RST missing %q:\n%s", want, got)
		}
	}
	for _, raw := range []string{".. rubric::", ".. toctree::", ".. include::", ".. list-table::", ".. only::"} {
		if strings.Contains(got, raw) {
			t.Errorf("converted RST retained directive %q:\n%s", raw, got)
		}
	}
}

func TestRSTToMarkdownConvertsNestedDirectivesOutsideCodeExamples(t *testing.T) {
	input := []byte(".. note::\n\n" +
		"   .. list-table:: Comparison\n\n" +
		"      * - .. rubric:: Cycles\n" +
		"        - .. figure:: /images/cycles.png\n\n" +
		".. code-block:: rst\n\n" +
		"   .. rubric:: Literal Example\n")
	got := string(rstToMarkdown(input))
	for _, want := range []string{"Table: Comparison", "Rubric: Cycles", "![Figure](/images/cycles.png)", ".. rubric:: Literal Example"} {
		if !strings.Contains(got, want) {
			t.Errorf("converted nested RST missing %q:\n%s", want, got)
		}
	}
	outsideFence := strings.Split(got, "```rst")[0]
	if strings.Contains(outsideFence, ".. list-table::") || strings.Contains(outsideFence, ".. rubric::") || strings.Contains(outsideFence, ".. figure::") {
		t.Errorf("converted nested RST retained raw directive outside fenced example:\n%s", got)
	}
}

func TestRSTToMarkdownConsumesFigureAndCodeOptions(t *testing.T) {
	input := []byte(".. figure:: /images/render.png\n" +
		"   :align: center\n" +
		"   :alt: Render result\n\n" +
		"   The final render.\n\n" +
		".. code-block:: python\n" +
		"   :linenos:\n\n" +
		"   bpy.ops.render.render()\n")
	got := string(rstToMarkdown(input))
	for _, want := range []string{
		"![Render result](/images/render.png)",
		"The final render.",
		"```python\nbpy.ops.render.render()\n```",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("converted RST missing %q:\n%s", want, got)
		}
	}
	for _, raw := range []string{":align:", ":alt:", ":linenos:"} {
		if strings.Contains(got, raw) {
			t.Errorf("converted RST retained directive option %q:\n%s", raw, got)
		}
	}
}
