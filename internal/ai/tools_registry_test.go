package ai

import "testing"

func TestToolsReferenceFor_Unregistered(t *testing.T) {
	if _, ok := ToolsReferenceFor("nope"); ok {
		t.Error("expected ok=false for an unregistered source")
	}
}

func TestRegisterToolsReference_RegisterAndLookup(t *testing.T) {
	t.Cleanup(ResetToolsReferences)
	RegisterToolsReference("fake", "fake verb docs")

	got, ok := ToolsReferenceFor("fake")
	if !ok || got != "fake verb docs" {
		t.Errorf("ToolsReferenceFor(fake) = (%q, %v), want (%q, true)", got, ok, "fake verb docs")
	}
}

func TestRegisterToolsReference_PanicsOnEmptySource(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for an empty source")
		}
	}()
	RegisterToolsReference("", "text")
}

func TestRegisterToolsReference_PanicsOnEmptyText(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for empty text")
		}
	}()
	RegisterToolsReference("fake-empty-text", "")
}

func TestRegisterToolsReference_PanicsOnCoreSource(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic when registering a core source")
		}
	}()
	RegisterToolsReference("github", "text")
}

func TestRegisterToolsReference_PanicsOnDuplicate(t *testing.T) {
	t.Cleanup(ResetToolsReferences)
	RegisterToolsReference("fake-dup", "text")
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate registration")
		}
	}()
	RegisterToolsReference("fake-dup", "text")
}
