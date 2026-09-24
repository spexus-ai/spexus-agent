package swarm

import (
	"encoding/json"
	"testing"
)

func TestTextProfileToolAllowlist(t *testing.T) {
	profile := TextProfile{ID: "coder", Model: "provider/model", Reasoning: "low", Prompt: "Work in the mounted workspace", Tools: []string{"read", "edit", "write", "bash"}, Extensions: []string{}}
	encode := func(p TextProfile) []byte {
		t.Helper()
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if _, err := ValidateTextProfile(encode(profile)); err != nil {
		t.Fatalf("built-in coding tools rejected: %v", err)
	}
	profile.Tools = []string{"read", "read"}
	if _, err := ValidateTextProfile(encode(profile)); err == nil {
		t.Fatal("duplicate tool accepted")
	}
	profile.Tools = []string{"network"}
	if _, err := ValidateTextProfile(encode(profile)); err == nil {
		t.Fatal("unknown tool accepted")
	}
	profile.Tools = []string{"read"}
	profile.Extensions = []string{"/tmp/extension.ts"}
	if _, err := ValidateTextProfile(encode(profile)); err == nil {
		t.Fatal("extension accepted")
	}
}
