package inputanswer

import (
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
)

func TestValidateUsesQuestionOptionAndFreeTextContract(t *testing.T) {
	request := &state.Request{Options: []state.Option{{ID: "safe", Label: "Safe"}}}
	if err := Validate(request, "safe", nil); err != nil {
		t.Fatal(err)
	}
	if err := Validate(request, "other", nil); err == nil {
		t.Fatal("unadvertised option was accepted")
	}
	request.AllowFreeText = true
	if err := Validate(request, "a documented alternative", nil); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsBoundariesAndSecrets(t *testing.T) {
	request := &state.Request{AllowFreeText: true}
	for name, answer := range map[string]string{
		"empty":    "",
		"control":  "ok\x00no",
		"oversize": strings.Repeat("a", MaxAnswerBytes+1),
		"secret":   "prefix known-secret-value suffix",
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(request, answer, []string{"known-secret-value"}); err == nil {
				t.Fatalf("%s answer was accepted", name)
			}
		})
	}
}
