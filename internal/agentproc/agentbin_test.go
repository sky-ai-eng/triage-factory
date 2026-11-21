package agentproc

import (
	"runtime"
	"strings"
	"testing"
)

// TestLocalAgentBinaryNamesTheSameThingInPromptAndAllowlist pins that the
// string the prompt tells the model to run and the one the allowlist grants
// come from one function, so a rule can never name a binary the prompt does
// not.
func TestLocalAgentBinaryNamesTheSameThingInPromptAndAllowlist(t *testing.T) {
	host := `C:\Users\me\AppData\Local\Programs\triagefactory\triagefactory.exe`
	if runtime.GOOS != "windows" {
		host = "/usr/local/bin/triagefactory"
	}
	name := LocalAgentBinary(host)

	allow := BuildAllowedToolsFor(AllowedToolsOptions{SelfBin: name})
	if want := "Bash(" + name + " exec *)"; !strings.Contains(allow, want) {
		t.Errorf("allowlist lacks %q: %.200s", want, allow)
	}
	if runtime.GOOS == "windows" {
		if name != "triagefactory" {
			t.Errorf("LocalAgentBinary = %q, want the bare name triagefactory", name)
		}
		if strings.Contains(allow, `\`) {
			t.Errorf("allowlist carries a backslash path that a bash command would never match: %.300s", allow)
		}
	} else if name != host {
		t.Errorf("LocalAgentBinary = %q, want the host path %q unchanged", name, host)
	}
}

func TestAgentVisiblePath(t *testing.T) {
	in := `C:\Users\me\.triagefactory\runs\abc\owner\repo\slug`
	want := "C:/Users/me/.triagefactory/runs/abc/owner/repo/slug"
	if runtime.GOOS != "windows" {
		in, want = "/home/me/runs/abc", "/home/me/runs/abc"
	}
	if got := AgentVisiblePath(in); got != want {
		t.Errorf("AgentVisiblePath(%q) = %q, want %q", in, got, want)
	}
}
