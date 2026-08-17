package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
)

const InboxVersion = 1

var deliveryIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)
var commitSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{40,64}$`)

var allowedActions = map[string]map[string]bool{
	"issues": {
		"labeled": true, "unlabeled": true, "opened": true, "reopened": true,
		"closed": true, "edited": true,
	},
	"issue_comment": {"created": true, "edited": true, "deleted": true},
	"pull_request": {
		"opened": true, "ready_for_review": true, "synchronize": true,
		"converted_to_draft": true, "closed": true, "reopened": true,
	},
	"check_run":    {"created": true, "rerequested": true, "completed": true},
	"status":       {"pending": true, "success": true, "failure": true, "error": true},
	"workflow_run": {"requested": true, "in_progress": true, "completed": true},
	"ping":         {"": true},
}

type Registration struct {
	Entry  registry.Entry
	Config config.Config
}

type Delivery struct {
	Version           int       `json:"version"`
	DeliveryID        string    `json:"delivery_id"`
	Event             string    `json:"event"`
	Action            string    `json:"action,omitempty"`
	RepoID            string    `json:"repo_id"`
	RepositoryID      int64     `json:"repository_id"`
	Repository        string    `json:"repository"`
	InstallationID    int64     `json:"installation_id,omitempty"`
	IssueNumber       int       `json:"issue_number,omitempty"`
	PullRequestNumber int       `json:"pull_request_number,omitempty"`
	HeadSHA           string    `json:"head_sha,omitempty"`
	AcceptedAt        time.Time `json:"accepted_at"`
	RoutedAt          time.Time `json:"routed_at,omitempty"`
}

type Status struct {
	Version                   int       `json:"version"`
	Mode                      string    `json:"mode"`
	ListenerAddress           string    `json:"listener_address"`
	LastAcceptedDelivery      string    `json:"last_accepted_delivery,omitempty"`
	LastAcceptedAt            time.Time `json:"last_accepted_at,omitempty"`
	QueueDepth                int       `json:"queue_depth"`
	Accepted                  uint64    `json:"accepted"`
	Duplicates                uint64    `json:"duplicates"`
	Rejected                  uint64    `json:"rejected"`
	LastSuccessfulSafetySweep time.Time `json:"last_successful_safety_sweep,omitempty"`
	NotModified304            uint64    `json:"not_modified_304"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

type Broker struct {
	Root          string
	Registrations []Registration
	Logger        *log.Logger
	Now           func() time.Time

	mu       sync.Mutex
	status   Status
	byName   map[string]Registration
	byID     map[int64]Registration
	work     chan string
	requests chan struct{}
	sweeps   chan struct{}
	server   *http.Server
}

func (b *Broker) Run(ctx context.Context) error {
	if err := b.initialize(); err != nil {
		return err
	}
	address, limits, err := commonListener(b.Registrations)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen for GitHub webhook on %s: %w", address, err)
	}
	defer listener.Close()
	host, _, _ := net.SplitHostPort(listener.Addr().String())
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("refusing non-loopback webhook listener %s", listener.Addr())
	}
	b.mu.Lock()
	b.status.ListenerAddress = listener.Addr().String()
	b.persistStatusLocked()
	b.mu.Unlock()

	b.server = &http.Server{
		Handler: http.HandlerFunc(b.ServeHTTP), ReadTimeout: limits.ReadTimeout.Duration,
		ReadHeaderTimeout: limits.ReadHeaderTimeout.Duration, IdleTimeout: limits.IdleTimeout.Duration,
		MaxHeaderBytes: 32 * 1024,
	}
	for i := 0; i < limits.MaxConcurrent; i++ {
		go b.worker(ctx)
	}
	b.startSafetySweeps(ctx)
	go b.replay(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- b.server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return b.server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (b *Broker) initialize() error {
	if len(b.Registrations) == 0 {
		return errors.New("no webhook repositories are registered")
	}
	if b.Now == nil {
		b.Now = func() time.Time { return time.Now().UTC() }
	}
	if b.Logger == nil {
		b.Logger = log.New(io.Discard, "", 0)
	}
	b.byName = map[string]Registration{}
	b.byID = map[int64]Registration{}
	b.work = make(chan string, 1024)
	_, limits, err := commonListener(b.Registrations)
	if err != nil {
		return err
	}
	b.requests = make(chan struct{}, limits.MaxConcurrent)
	sweepConcurrency := limits.MaxConcurrent
	if sweepConcurrency > 4 {
		sweepConcurrency = 4
	}
	b.sweeps = make(chan struct{}, sweepConcurrency)
	b.status = Status{Version: InboxVersion, Mode: "webhook"}
	for _, registration := range b.Registrations {
		if !registration.Config.Webhook.Enabled() {
			continue
		}
		name := strings.ToLower(registration.Config.GitHub.Repo)
		if _, exists := b.byName[name]; exists {
			return fmt.Errorf("duplicate webhook repository registration %s", name)
		}
		if _, exists := b.byID[registration.Config.GitHub.RepositoryID]; exists {
			return fmt.Errorf("duplicate webhook repository ID %d", registration.Config.GitHub.RepositoryID)
		}
		b.byName[name] = registration
		b.byID[registration.Config.GitHub.RepositoryID] = registration
	}
	for _, dir := range []string{b.inboxDir(), filepath.Join(b.Root, "broker")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	b.refreshDepthLocked()
	b.persistStatusLocked()
	return nil
}

func (b *Broker) startSafetySweeps(ctx context.Context) {
	for _, registration := range b.Registrations {
		registration := registration
		if registration.Config.Webhook.Enabled() {
			go b.runSafetySweep(ctx, registration)
		}
	}
}

func (b *Broker) runSafetySweep(ctx context.Context, registration Registration) {
	interval := registration.Config.Webhook.SafetySweepInterval.Duration
	delay := jitterDuration(interval, registration.Config.Webhook.SafetySweepJitter)
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		select {
		case b.sweeps <- struct{}{}:
		case <-ctx.Done():
			return
		}
		next, err := b.sweepOnce(ctx, registration)
		<-b.sweeps
		if err != nil {
			b.Logger.Printf("conditional GitHub safety sweep failed for repo_id=%s", registration.Entry.RepoID)
			delay = 5 * time.Minute
			if delay > interval {
				delay = interval
			}
			continue
		}
		delay = jitterDuration(interval, registration.Config.Webhook.SafetySweepJitter)
		if next > delay {
			delay = next
		}
	}
}

func (b *Broker) sweepOnce(ctx context.Context, registration Registration) (time.Duration, error) {
	repoDir := filepath.Join(b.Root, "repos", registration.Entry.RepoID)
	state, err := LoadSweepState(repoDir)
	if err != nil {
		return 0, err
	}
	client := gh.CLI{Path: registration.Entry.Commands["gh"], Secrets: registration.Config.RedactionValues()}
	result, err := client.ListReadyConditional(ctx, registration.Config, state.ETag, state.LastModified)
	if err != nil {
		return 0, err
	}
	now := b.Now()
	state.LastStatus = result.StatusCode
	state.LastSuccessful = now
	if result.ETag != "" {
		state.ETag = result.ETag
	}
	if result.LastModified != "" {
		state.LastModified = result.LastModified
	}
	state.RateRemaining = result.RateRemaining
	state.RateReset = result.RateReset
	if result.NotModified {
		state.NotModified304++
	} else if result.StatusCode == http.StatusOK {
		state.REST200++
	}
	if err := SaveSweepState(repoDir, state); err != nil {
		return 0, err
	}
	for _, issue := range result.Issues {
		delivery := Delivery{
			Version: InboxVersion, DeliveryID: fmt.Sprintf("sweep-%d-%d", now.UnixNano(), issue.Number),
			Event: "issues", Action: "reconciled", RepoID: registration.Entry.RepoID,
			RepositoryID: registration.Config.GitHub.RepositoryID, Repository: registration.Config.GitHub.Repo,
			IssueNumber: issue.Number, AcceptedAt: now,
		}
		if err := EnqueueMailbox(repoDir, delivery); err != nil {
			return 0, err
		}
	}
	b.mu.Lock()
	b.status.LastSuccessfulSafetySweep = now
	if result.NotModified {
		b.status.NotModified304++
	}
	b.status.UpdatedAt = now
	b.persistStatusLocked()
	b.mu.Unlock()
	if result.RateRemaining == "0" {
		if reset, parseErr := strconv.ParseInt(result.RateReset, 10, 64); parseErr == nil {
			resetAt := time.Unix(reset, 0).UTC().Add(time.Second)
			if resetAt.After(now) {
				return resetAt.Sub(now), nil
			}
		}
	}
	return 0, nil
}

func jitterDuration(base time.Duration, ratio float64) time.Duration {
	if ratio <= 0 {
		return base
	}
	factor := 1 - ratio + 2*ratio*rand.Float64()
	return time.Duration(float64(base) * factor)
}

func commonListener(registrations []Registration) (string, config.Webhook, error) {
	var selected config.Webhook
	for _, registration := range registrations {
		if !registration.Config.Webhook.Enabled() {
			continue
		}
		if selected.ListenerAddress == "" {
			selected = registration.Config.Webhook
			continue
		}
		other := registration.Config.Webhook
		if other.ListenerAddress != selected.ListenerAddress || other.MaxBodyBytes != selected.MaxBodyBytes ||
			other.ReadTimeout != selected.ReadTimeout || other.ReadHeaderTimeout != selected.ReadHeaderTimeout ||
			other.IdleTimeout != selected.IdleTimeout || other.MaxConcurrent != selected.MaxConcurrent {
			return "", config.Webhook{}, errors.New("all webhook repositories in a managed root must use the same listener and HTTP limits")
		}
	}
	if selected.ListenerAddress == "" {
		return "", config.Webhook{}, errors.New("no webhook repositories are registered")
	}
	return selected.ListenerAddress, selected, nil
}

type payloadEnvelope struct {
	Action     string `json:"action"`
	Repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Issue struct {
		Number int `json:"number"`
	} `json:"issue"`
	PullRequest struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	CheckRun struct {
		HeadSHA      string `json:"head_sha"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_run"`
	WorkflowRun struct {
		HeadSHA      string `json:"head_sha"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"workflow_run"`
	SHA   string `json:"sha"`
	State string `json:"state"`
}

func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/github/webhook" {
		http.NotFound(w, r)
		return
	}
	select {
	case b.requests <- struct{}{}:
		defer func() { <-b.requests }()
	default:
		b.reject(w, http.StatusServiceUnavailable)
		return
	}
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	event := r.Header.Get("X-GitHub-Event")
	if !deliveryIDPattern.MatchString(deliveryID) || allowedActions[event] == nil {
		b.reject(w, http.StatusBadRequest)
		return
	}
	maxBody, err := b.maxBodyBytes()
	if err != nil {
		b.reject(w, http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		b.reject(w, http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxBody {
		b.reject(w, http.StatusRequestEntityTooLarge)
		return
	}
	var payload payloadEnvelope
	if err := json.Unmarshal(body, &payload); err != nil {
		b.reject(w, http.StatusBadRequest)
		return
	}
	registration, ok := b.byName[strings.ToLower(payload.Repository.FullName)]
	if !ok || registration.Config.GitHub.RepositoryID != payload.Repository.ID {
		b.reject(w, http.StatusForbidden)
		return
	}
	if !allowedActions[event][actionFor(event, payload)] {
		b.reject(w, http.StatusUnprocessableEntity)
		return
	}
	if event != "ping" {
		if payload.Installation.ID == 0 {
			if !registration.Config.Webhook.AllowRepositoryHook {
				b.reject(w, http.StatusForbidden)
				return
			}
		} else if !containsInt64(registration.Config.Webhook.InstallationIDs, payload.Installation.ID) {
			b.reject(w, http.StatusForbidden)
			return
		}
	}
	if !verifyAnySignature(r.Header.Get("X-Hub-Signature-256"), body, registration.Config.Webhook.SecretSource, registration.Config.Webhook.PreviousSecret) {
		b.reject(w, http.StatusUnauthorized)
		return
	}
	if !payloadTargetsValid(event, payload) {
		b.reject(w, http.StatusUnprocessableEntity)
		return
	}
	delivery := Delivery{
		Version: InboxVersion, DeliveryID: deliveryID, Event: event, Action: actionFor(event, payload),
		RepoID: registration.Entry.RepoID, RepositoryID: payload.Repository.ID,
		Repository: registration.Config.GitHub.Repo, InstallationID: payload.Installation.ID,
		IssueNumber: payload.Issue.Number, PullRequestNumber: payload.PullRequest.Number,
		HeadSHA: payload.PullRequest.Head.SHA, AcceptedAt: b.Now(),
	}
	if delivery.PullRequestNumber == 0 && len(payload.CheckRun.PullRequests) > 0 {
		delivery.PullRequestNumber = payload.CheckRun.PullRequests[0].Number
		delivery.HeadSHA = payload.CheckRun.HeadSHA
	}
	if delivery.PullRequestNumber == 0 && len(payload.WorkflowRun.PullRequests) > 0 {
		delivery.PullRequestNumber = payload.WorkflowRun.PullRequests[0].Number
		delivery.HeadSHA = payload.WorkflowRun.HeadSHA
	}
	if delivery.HeadSHA == "" {
		delivery.HeadSHA = payload.SHA
	}
	created, err := b.appendInbox(delivery)
	if err != nil {
		b.reject(w, http.StatusInternalServerError)
		return
	}
	if created {
		select {
		case b.work <- deliveryID:
		default:
		}
		b.accept(delivery)
	} else {
		b.duplicate()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, `{"accepted":true}`)
}

func payloadTargetsValid(event string, payload payloadEnvelope) bool {
	switch event {
	case "issues", "issue_comment":
		return payload.Issue.Number > 0
	case "pull_request":
		return payload.PullRequest.Number > 0 && commitSHAPattern.MatchString(payload.PullRequest.Head.SHA)
	case "check_run":
		return commitSHAPattern.MatchString(payload.CheckRun.HeadSHA)
	case "status":
		return commitSHAPattern.MatchString(payload.SHA)
	case "workflow_run":
		return commitSHAPattern.MatchString(payload.WorkflowRun.HeadSHA)
	case "ping":
		return true
	default:
		return false
	}
}

func actionFor(event string, payload payloadEnvelope) string {
	if event == "status" {
		return payload.State
	}
	return payload.Action
}

func (b *Broker) maxBodyBytes() (int64, error) {
	_, limits, err := commonListener(b.Registrations)
	return limits.MaxBodyBytes, err
}

func verifyAnySignature(header string, body []byte, sources ...config.SecretSource) bool {
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	valid := false
	for _, source := range sources {
		secret, err := readSecret(source)
		if err != nil || len(secret) == 0 {
			continue
		}
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write(body)
		valid = hmac.Equal(provided, mac.Sum(nil)) || valid
	}
	return valid
}

func readSecret(source config.SecretSource) ([]byte, error) {
	if source.Env != "" {
		value, ok := os.LookupEnv(source.Env)
		if !ok || value == "" {
			return nil, fmt.Errorf("secret environment variable is unavailable")
		}
		return []byte(value), nil
	}
	if source.File == "" {
		return nil, errors.New("secret source is empty")
	}
	info, err := os.Lstat(source.File)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("secret file must be regular and accessible only by its owner")
	}
	value, err := os.ReadFile(source.File)
	return []byte(strings.TrimSpace(string(value))), err
}

func (b *Broker) appendInbox(delivery Delivery) (bool, error) {
	path := b.inboxPath(delivery.DeliveryID)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(delivery); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return false, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return false, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return false, err
	}
	dir, err := os.Open(b.inboxDir())
	if err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return true, nil
}

func (b *Broker) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-b.work:
			if err := b.route(id); err != nil {
				b.Logger.Printf("webhook delivery routing failed for delivery_id=%s", id)
			}
		}
	}
}

func (b *Broker) replay(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		entries, _ := os.ReadDir(b.inboxDir())
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				id := strings.TrimSuffix(entry.Name(), ".json")
				data, readErr := os.ReadFile(filepath.Join(b.inboxDir(), entry.Name()))
				var delivery Delivery
				if readErr != nil || json.Unmarshal(data, &delivery) != nil || !delivery.RoutedAt.IsZero() {
					continue
				}
				select {
				case b.work <- id:
				default:
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Broker) route(id string) error {
	path := b.inboxPath(id)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var delivery Delivery
	if err := json.Unmarshal(data, &delivery); err != nil {
		return err
	}
	if !delivery.RoutedAt.IsZero() {
		return nil
	}
	registration, ok := b.byName[strings.ToLower(delivery.Repository)]
	if !ok || registration.Entry.RepoID != delivery.RepoID || registration.Config.GitHub.RepositoryID != delivery.RepositoryID {
		return errors.New("delivery registration no longer matches")
	}
	mailbox := filepath.Join(b.Root, "repos", delivery.RepoID, "webhook-mailbox")
	if err := os.MkdirAll(mailbox, 0o700); err != nil {
		return err
	}
	if err := fsutil.WriteJSON(filepath.Join(mailbox, delivery.DeliveryID+".json"), delivery, 0o600); err != nil {
		return err
	}
	delivery.RoutedAt = b.Now()
	if err := fsutil.WriteJSON(path, delivery, 0o600); err != nil {
		return err
	}
	b.mu.Lock()
	b.refreshDepthLocked()
	b.persistStatusLocked()
	b.mu.Unlock()
	return nil
}

func (b *Broker) inboxDir() string           { return filepath.Join(b.Root, "broker", "inbox") }
func (b *Broker) inboxPath(id string) string { return filepath.Join(b.inboxDir(), id+".json") }
func (b *Broker) statusPath() string         { return filepath.Join(b.Root, "broker", "status.json") }

func (b *Broker) reject(w http.ResponseWriter, code int) {
	b.mu.Lock()
	b.status.Rejected++
	b.status.UpdatedAt = b.Now()
	b.persistStatusLocked()
	b.mu.Unlock()
	http.Error(w, http.StatusText(code), code)
}

func (b *Broker) accept(delivery Delivery) {
	b.mu.Lock()
	b.status.Accepted++
	b.status.LastAcceptedDelivery = delivery.DeliveryID
	b.status.LastAcceptedAt = delivery.AcceptedAt
	b.refreshDepthLocked()
	b.persistStatusLocked()
	b.mu.Unlock()
}

func (b *Broker) duplicate() {
	b.mu.Lock()
	b.status.Duplicates++
	b.status.UpdatedAt = b.Now()
	b.persistStatusLocked()
	b.mu.Unlock()
}

func (b *Broker) refreshDepthLocked() {
	entries, _ := os.ReadDir(b.inboxDir())
	b.status.QueueDepth = 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(b.inboxDir(), entry.Name()))
		var delivery Delivery
		if err == nil && json.Unmarshal(data, &delivery) == nil && delivery.RoutedAt.IsZero() {
			b.status.QueueDepth++
		}
	}
	b.status.UpdatedAt = b.Now()
}

func (b *Broker) persistStatusLocked() {
	_ = fsutil.WriteJSON(b.statusPath(), b.status, 0o600)
}

func containsInt64(values []int64, target int64) bool {
	index := sort.Search(len(values), func(i int) bool { return values[i] >= target })
	if index < len(values) && values[index] == target {
		return true
	}
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
