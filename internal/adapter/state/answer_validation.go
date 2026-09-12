package state

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
)

const MaxAnswerBytes = 16 * 1024

func ValidateAnswer(request *Request, answer string, secrets []string) error {
	if strings.TrimSpace(answer) == "" {
		return fmt.Errorf("answer must not be empty")
	}
	if len(answer) > MaxAnswerBytes {
		return fmt.Errorf("answer must not exceed %d bytes", MaxAnswerBytes)
	}
	if !utf8.ValidString(answer) {
		return fmt.Errorf("answer must be valid UTF-8")
	}
	for _, r := range answer {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("answer must not contain control characters")
		}
	}
	if redact.StringWithSecrets(answer, secrets) != answer {
		return fmt.Errorf("answer must not contain a credential or configured secret")
	}
	if request != nil && len(request.Options) > 0 {
		for _, option := range request.Options {
			if answer == option.ID {
				return nil
			}
		}
		if !request.AllowFreeText {
			return fmt.Errorf("answer must be one of the advertised option IDs")
		}
	}
	return nil
}

func BodyDigest(body string) string {
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:])
}
