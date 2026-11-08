package cpa

import (
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestResolveAIStudioDisplayName(t *testing.T) {
	// 1. When Details.Alias is present and valid, it takes priority
	modelWithAlias := aistudio.Model{
		ID:   "models/gemini-3.1-flash-image",
		Name: "Nano Banana 2",
		Details: &aistudio.ModelDetails{
			Alias: "Gemini 3.1 Flash Image",
		},
	}
	got := resolveAIStudioDisplayName(modelWithAlias, "gemini-3.1-flash-image")
	if got != "Gemini 3.1 Flash Image" {
		t.Errorf("expected %q, got %q", "Gemini 3.1 Flash Image", got)
	}

	// 2. When Details.Alias is a pointer ("Points to ..."), ignore it and fallback to Name
	pointerModel := aistudio.Model{
		ID:   "models/gemini-flash-latest",
		Name: "Gemini Flash Latest",
		Details: &aistudio.ModelDetails{
			Alias: "Points to gemini-3.8-flash",
		},
	}
	gotPointer := resolveAIStudioDisplayName(pointerModel, "gemini-flash-latest")
	if gotPointer != "Gemini Flash Latest" {
		t.Errorf("expected %q, got %q", "Gemini Flash Latest", gotPointer)
	}

	// 3. When Details is nil, fall back to cleanAIStudioDisplayName
	nilDetailsModel := aistudio.Model{
		ID:   "models/gemini-3-pro-image",
		Name: "Nano Banana Pro",
	}
	gotFallback := resolveAIStudioDisplayName(nilDetailsModel, "gemini-3-pro-image")
	if gotFallback != "Gemini 3 Pro Image" {
		t.Errorf("expected %q, got %q", "Gemini 3 Pro Image", gotFallback)
	}
}

func TestCleanAIStudioDisplayName(t *testing.T) {
	tests := []struct {
		id       string
		rawName  string
		expected string
	}{
		{"gemini-3.1-flash-image", "Nano Banana 2", "Gemini 3.1 Flash Image"},
		{"models/gemini-3.1-flash-image", "Nano Banana 2", "Gemini 3.1 Flash Image"},
		{"gemini-3-pro-image", "Nano Banana Pro", "Gemini 3 Pro Image"},
		{"gemini-3.1-flash-lite-image", "Nano Banana 2 Lite", "Gemini 3.1 Flash Lite Image"},
		{"gemini-nano-banana-2.1", "Nano Banana 2.1", "Gemini Nano Banana 2.1"},
		{"gemini-2.5-flash-image", "Nano Banana", "Gemini 2.5 Flash Image"},
		{"gemini-2.5-flash-image-preview", "Gemini 2.5 Flash Image Preview (Nano Banana)", "Gemini 2.5 Flash Image Preview"},
		{"gemini-3.8-flash", "Gemini 3.8 Flash", "Gemini 3.8 Flash"},
		{"custom-unknown-model", "", "custom-unknown-model"},
	}

	for _, tc := range tests {
		actual := cleanAIStudioDisplayName(tc.id, tc.rawName)
		if actual != tc.expected {
			t.Errorf("cleanAIStudioDisplayName(%q, %q) = %q, expected %q", tc.id, tc.rawName, actual, tc.expected)
		}
		if strings.Contains(actual, "(") || strings.Contains(actual, ")") {
			t.Errorf("cleanAIStudioDisplayName(%q, %q) = %q contains parentheses", tc.id, tc.rawName, actual)
		}
	}
}

func TestDefaultFallbackModels(t *testing.T) {
	models := defaultFallbackModels()
	if len(models) == 0 {
		t.Fatalf("expected non-empty defaultFallbackModels")
	}

	hasEmbedding := false
	hasPro := false
	for _, m := range models {
		if m.ID == "models/gemini-2.5-pro" {
			hasPro = true
		}
		if m.ID == "models/gemini-embedding-001" {
			hasEmbedding = true
		}
		if strings.Contains(m.Name, "(") || strings.Contains(m.Name, ")") {
			t.Errorf("model name %q contains parentheses", m.Name)
		}
	}

	if !hasEmbedding {
		t.Errorf("expected defaultFallbackModels to include models/gemini-embedding-001")
	}
	if !hasPro {
		t.Errorf("expected defaultFallbackModels to include models/gemini-2.5-pro")
	}
}

func TestCleanAIStudioDescription(t *testing.T) {
	embDesc := cleanAIStudioDescription("gemini-embedding-001", "")
	if !strings.Contains(embDesc, "向量嵌入") {
		t.Errorf("expected embedding description to explain embedding usage, got: %q", embDesc)
	}

	bananaDesc := cleanAIStudioDescription("gemini-3.1-flash-image", "")
	if !strings.Contains(bananaDesc, "Nano Banana 2") {
		t.Errorf("expected banana description to reference internal codename, got: %q", bananaDesc)
	}
}
