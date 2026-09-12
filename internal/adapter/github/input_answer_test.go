package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
)

func answerTestMarker(prefix string, payload any) string {
	data, _ := json.Marshal(payload)
	return prefix + "payload=" + base64.RawURLEncoding.EncodeToString(data) + " digest=" + state.BodyDigest(string(data)) + " -->"
}

func answerTestQuestion(id string, issue int, created time.Time) string {
	payload := inputRequestMarkerPayload{Version: 1, RequestID: id, IssueNumber: issue, RunID: "run_1", Question: "Choose?", Options: []state.Option{{ID: "safe", Label: "Safe"}}, CreatedAt: created}
	return renderInputRequest(answerTestMarker("<!-- codex-issue-loop:input-request:v1 request="+id+" ", payload), "<!-- codex-issue-loop:request:"+id+" -->", payload)
}

func answerTestComments(t *testing.T, path string, comments []InputComment) {
	t.Helper()
	raw := []map[string]any{}
	for _, c := range comments {
		raw = append(raw, map[string]any{"id": c.ID, "body": c.Body, "created_at": c.CreatedAt, "updated_at": c.UpdatedAt, "user": map[string]any{"id": c.ActorID, "login": c.Actor, "type": c.ActorType}})
	}
	data, err := json.Marshal([]any{raw})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFaultGitHubAnswerSubmissionAndAuthenticatedReceipt(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprint(disconnect), func(t *testing.T) {
			dir := t.TempDir()
			commentsPath, nextPath, postedPath := filepath.Join(dir, "comments"), filepath.Join(dir, "next"), filepath.Join(dir, "posted")
			script := filepath.Join(dir, "gh")
			exit := "printf '%s' '{\"id\":42}'"
			if disconnect {
				exit = "exit 1"
			}
			fake := fmt.Sprintf(`#!/bin/sh
case "$*" in
 *"--method POST"*) cat > %q; cp %q %q; %s ;;
 *"/user"*) printf '%%s' '{"id":7,"login":"loop","type":"User"}' ;;
 *"/comments?per_page=100"*) cat %q ;;
 *"/repos/owner/repo/issues/1"*) printf '%%s' '{"number":1,"state":"open"}' ;;
 *) exit 2 ;;
esac
`, postedPath, nextPath, commentsPath, exit, commentsPath)
			if err := os.WriteFile(script, []byte(fake), 0700); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Add(-time.Minute)
			question := InputComment{ID: 1, Body: answerTestQuestion("req_1", 1, now), ActorID: 7, Actor: "loop", ActorType: "User", CreatedAt: now, UpdatedAt: now}
			answer := InputComment{ID: 42, Body: "/agent-loop answer req_1 safe", ActorID: 7, Actor: "loop", ActorType: "User", CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)}
			answerTestComments(t, commentsPath, []InputComment{question})
			answerTestComments(t, nextPath, []InputComment{question, answer})
			client := CLI{Path: script}
			result, err := client.SubmitInputAnswer(context.Background(), "owner/repo", 1, "req_1", "safe")
			if err != nil || result.Status != "submitted" || result.CommentID != 42 || result.Recorded != "unknown" || result.Execution != "unknown" || result.CommentURL != "https://github.com/owner/repo/issues/1#issuecomment-42" {
				t.Fatalf("%+v %v", result, err)
			}
			data, _ := os.ReadFile(postedPath)
			var posted map[string]string
			if json.Unmarshal(data, &posted) != nil || posted["body"] != answer.Body {
				t.Fatal("incorrect answer wire format")
			}
			if err := os.Remove(postedPath); err != nil {
				t.Fatal(err)
			}
			if _, err := client.SubmitInputAnswer(context.Background(), "owner/repo", 1, "req_1", "safe"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(postedPath); !os.IsNotExist(err) {
				t.Fatal("duplicate POST")
			}
			if _, err := client.SubmitInputAnswer(context.Background(), "owner/repo", 1, "req_1", "other"); err == nil {
				t.Fatal("conflicting answer allowed")
			}
			result, err = client.CheckInputAnswer(context.Background(), "owner/repo", 1, "req_1", 0)
			if err != nil || result.Status != "submitted" || result.CommentID != 42 {
				t.Fatalf("lookup: %+v %v", result, err)
			}
			ack := InputComment{ID: 43, ActorID: 8, Actor: "admin", ActorType: "User", Body: answerTestMarker("<!-- codex-issue-loop:answer-ack:v1 comment=42 ", map[string]any{"version": 1, "request_id": "req_1", "comment_id": 42, "outcome": "accepted"})}
			answerTestComments(t, commentsPath, []InputComment{question, answer, ack})
			result, err = client.CheckInputAnswer(context.Background(), "owner/repo", 1, "req_1", 42)
			if err != nil || result.Recorded != "unknown" {
				t.Fatalf("forged receipt: %+v %v", result, err)
			}
			ack.ActorID = 7
			ack.Actor = "loop"
			validAck := ack.Body
			ack.Body = answerTestMarker("<!-- codex-issue-loop:answer-ack:v1 comment=42 ", map[string]any{"version": 1, "request_id": "req_1", "comment_id": 99, "outcome": "accepted"})
			answerTestComments(t, commentsPath, []InputComment{question, answer, ack})
			if _, err := client.CheckInputAnswer(context.Background(), "owner/repo", 1, "req_1", 42); err == nil {
				t.Fatal("wrong comment identity accepted")
			}
			ack.Body = strings.Replace(validAck, "digest=", "digest=bad", 1)
			answerTestComments(t, commentsPath, []InputComment{question, answer, ack})
			if _, err := client.CheckInputAnswer(context.Background(), "owner/repo", 1, "req_1", 42); err == nil {
				t.Fatal("corrupt receipt accepted")
			}
			ack.Body = validAck

			newer := question
			newer.ID = 44
			newer.Body = answerTestQuestion("req_2", 1, now.Add(time.Second))
			answerTestComments(t, commentsPath, []InputComment{question, ack, newer})
			result, err = client.CheckInputAnswer(context.Background(), "owner/repo", 1, "req_1", 42)
			if err != nil || result.Recorded != "true" || result.Execution != "unknown" {
				t.Fatalf("deleted source receipt after new question: %+v %v", result, err)
			}
			ack.Body = answerTestMarker("<!-- codex-issue-loop:answer-ack:v1 comment=42 ", map[string]any{"version": 1, "request_id": "req_other", "comment_id": 42, "outcome": "accepted"})
			answerTestComments(t, commentsPath, []InputComment{question, answer, ack})
			if _, err := client.CheckInputAnswer(context.Background(), "owner/repo", 1, "req_1", 42); err == nil {
				t.Fatal("wrong request receipt accepted")
			}
		})
	}
}

func TestGitHubAnswerRejectsBeforePost(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []string{"option", "secret", "control", "utf8", "oversize", "forged", "tampered", "legacy tampered", "other issue", "stale", "unknown", "managed", "wrong run", "duplicate question"} {
		t.Run(test, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "gh")
			commentsPath := filepath.Join(dir, "comments")
			postedPath := filepath.Join(dir, "posted")
			fake := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *\"--method POST\"*) touch %q; exit 1;; *\"/user\"*) printf '%%s' '{\"id\":7,\"login\":\"loop\",\"type\":\"User\"}';; *\"/comments?per_page=100\"*) cat %q;; *) exit 2;; esac\n", postedPath, commentsPath)
			if err := os.WriteFile(script, []byte(fake), 0700); err != nil {
				t.Fatal(err)
			}
			question := InputComment{ID: 1, Body: answerTestQuestion("req_1", 1, now), ActorID: 7, Actor: "loop", ActorType: "User", CreatedAt: now, UpdatedAt: now}
			answer := "safe"
			requestID := "req_1"
			comments := []InputComment{}
			switch test {
			case "option":
				answer = "unknown"
			case "secret":
				answer = "sensitive-fixture-value"
			case "control":
				answer = "a\x00b"
			case "utf8":
				answer = "\xff"
			case "oversize":
				answer = strings.Repeat("a", state.MaxAnswerBytes+1)
			case "forged":
				question.ActorID = 8
			case "tampered":
				question.Body += " extra"
			case "legacy tampered":
				var payload inputRequestMarkerPayload
				if err := decodeInputMarker(question.Body, "<!-- codex-issue-loop:input-request:v1 request=req_1 ", &payload); err != nil {
					t.Fatal(err)
				}
				marker, _, _ := strings.Cut(question.Body, "\n")
				question.Body = renderLegacyInputRequest(marker, "<!-- codex-issue-loop:request:req_1 -->", payload) + " extra"
			case "other issue":
				question.Body = answerTestQuestion("req_1", 2, now)
			case "stale":
				newer := question
				newer.ID = 2
				newer.Body = answerTestQuestion("req_2", 1, now.Add(time.Second))
				comments = append(comments, newer)
			case "unknown":
				requestID = "req_missing"
			case "managed":
				answer = "<!-- codex-issue-loop:answer-ack:v1 -->"
			case "wrong run":
				question.Body = strings.ReplaceAll(question.Body, "payload=", "payload=invalid")
			case "duplicate question":
				comments = append(comments, question)
			}
			comments = append(comments, question)
			answerTestComments(t, commentsPath, comments)
			_, err := (CLI{Path: script, Secrets: []string{"sensitive-fixture-value"}}).SubmitInputAnswer(context.Background(), "owner/repo", 1, requestID, answer)
			if err == nil {
				t.Fatal("invalid submission accepted")
			}
			if _, err := os.Stat(postedPath); !os.IsNotExist(err) {
				t.Fatal("invalid input posted")
			}
		})
	}
}

func TestFaultGitHubAnswerUnknownPostDoesNotRetry(t *testing.T) {
	dir := t.TempDir()
	script, commentsPath, postedPath := filepath.Join(dir, "gh"), filepath.Join(dir, "comments"), filepath.Join(dir, "posts")
	fake := fmt.Sprintf(`#!/bin/sh
case "$*" in
 *"--method POST"*) cat > /dev/null; printf 'post\n' >> %q; exit 1 ;;
 *"/user"*) printf '%%s' '{"id":7,"login":"loop","type":"User"}' ;;
 *"/comments?per_page=100"*) cat %q ;;
 *"/repos/owner/repo/issues/1"*) printf '%%s' '{"number":1,"state":"open"}' ;;
 *) exit 2 ;;
esac
`, postedPath, commentsPath)
	if err := os.WriteFile(script, []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	answerTestComments(t, commentsPath, []InputComment{{ID: 1, Body: answerTestQuestion("req_1", 1, now), ActorID: 7, Actor: "loop", ActorType: "User", CreatedAt: now, UpdatedAt: now}})
	client := CLI{Path: script}
	result, err := client.SubmitInputAnswer(context.Background(), "owner/repo", 1, "req_1", "safe")
	if err != nil || result.Status != "unknown" || result.Recorded != "unknown" {
		t.Fatalf("%+v %v", result, err)
	}
	result, err = client.CheckInputAnswer(context.Background(), "owner/repo", 1, "req_1", 0)
	if err != nil || result.Status != "unknown" {
		t.Fatalf("%+v %v", result, err)
	}
	data, err := os.ReadFile(postedPath)
	if err != nil || string(data) != "post\n" {
		t.Fatalf("POST repeated: %v", err)
	}
}
