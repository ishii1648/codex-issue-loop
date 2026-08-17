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

const (
	receiptRetention = 30 * 24 * time.Hour
	receiptLimit     = 200000
)

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
	SweepClient   func(Registration) gh.PagedConditionalQueueClient
	SweepTimers   SweepTimerSource
	ReadDir       func(string) ([]os.DirEntry, error)

	mu         sync.Mutex
	deliveryMu sync.Mutex
	status     Status
	byName     map[string]Registration
	byID       map[int64]Registration
	work       chan string
	requests   chan struct{}
	sweeps     chan struct{}
	server     *http.Server
}

type SweepTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type SweepTimerSource interface {
	NewTimer(time.Duration) SweepTimer
}

type systemSweepTimer struct{ timer *time.Timer }

func (t systemSweepTimer) C() <-chan time.Time { return t.timer.C }
func (t systemSweepTimer) Stop() bool          { return t.timer.Stop() }

type systemSweepTimers struct{}

func (systemSweepTimers) NewTimer(delay time.Duration) SweepTimer {
	return systemSweepTimer{timer: time.NewTimer(delay)}
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
	go b.heartbeat(ctx)
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
	for _, dir := range []string{b.inboxDir(), b.receiptDir(), filepath.Join(b.Root, "broker")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	if err := b.migrateRoutedInbox(); err != nil {
		return err
	}
	if err := b.compactReceipts(); err != nil {
		return err
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
	// Broker recovery closes the webhook outage window after a short jittered
	// delay. Keeping it non-zero avoids an extra interval-boundary request when
	// a fleet of brokers is restarted together.
	recoveryDelay := 5 * time.Second
	if interval < recoveryDelay {
		recoveryDelay = interval / 10
	}
	delay := jitterDuration(recoveryDelay, registration.Config.Webhook.SafetySweepJitter)
	for {
		timer := b.newSweepTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
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
	var client gh.PagedConditionalQueueClient = gh.CLI{Path: registration.Entry.Commands["gh"], Secrets: registration.Config.RedactionValues()}
	if b.SweepClient != nil {
		client = b.SweepClient(registration)
	}
	now := b.Now()
	previous := composeSweepIssues(state.Pages)
	targets := make(map[int]Delivery)
	notModified := uint64(0)
	for page := 1; page <= 10; page++ {
		cached := state.Pages[page]
		result, pageErr := client.ListReadyConditionalPage(ctx, registration.Config, page, cached.ETag, cached.LastModified)
		if pageErr != nil {
			return 0, pageErr
		}
		state.LastStatus = result.StatusCode
		state.RateRemaining = result.RateRemaining
		state.RateReset = result.RateReset
		if result.NotModified {
			state.NotModified304++
			notModified++
			if result.ETag != "" {
				cached.ETag = result.ETag
			}
			if result.LastModified != "" {
				cached.LastModified = result.LastModified
			}
			state.Pages[page] = cached
		} else if result.StatusCode == http.StatusOK {
			state.REST200++
			cached = SweepPageState{
				ETag: result.ETag, LastModified: result.LastModified,
				ItemCount: result.ItemCount, Issues: append([]gh.Issue(nil), result.Issues...),
			}
			state.Pages[page] = cached
			for _, issue := range result.Issues {
				targets[issue.Number] = b.sweepDelivery(registration, issue.Number, "reconciled", now)
			}
		}
		if page == 1 {
			state.ETag = cached.ETag
			state.LastModified = cached.LastModified
		}
		if cached.ItemCount < 100 {
			for stale := page + 1; stale <= 10; stale++ {
				delete(state.Pages, stale)
			}
			break
		}
	}
	current := composeSweepIssues(state.Pages)
	for number := range previous {
		if _, exists := current[number]; !exists {
			targets[number] = b.sweepDelivery(registration, number, "collection_exited", now)
		}
	}
	state.LastSuccessful = now
	numbers := make([]int, 0, len(targets))
	for number := range targets {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	// The mailbox is the recovery intent. Persist it before advancing the cache
	// so a crash can only replay an idempotent target, never lose the diff.
	for _, number := range numbers {
		if err := EnqueueMailbox(repoDir, targets[number]); err != nil {
			return 0, err
		}
	}
	if err := SaveSweepState(repoDir, state); err != nil {
		return 0, err
	}
	b.mu.Lock()
	b.status.LastSuccessfulSafetySweep = now
	b.status.NotModified304 += notModified
	b.status.UpdatedAt = now
	b.persistStatusLocked()
	b.mu.Unlock()
	if state.RateRemaining == "0" {
		if reset, parseErr := strconv.ParseInt(state.RateReset, 10, 64); parseErr == nil {
			resetAt := time.Unix(reset, 0).UTC().Add(time.Second)
			if resetAt.After(now) {
				return resetAt.Sub(now), nil
			}
		}
	}
	return 0, nil
}

func composeSweepIssues(pages map[int]SweepPageState) map[int]gh.Issue {
	result := make(map[int]gh.Issue)
	for _, page := range pages {
		for _, issue := range page.Issues {
			result[issue.Number] = issue
		}
	}
	return result
}

func (b *Broker) sweepDelivery(registration Registration, number int, action string, now time.Time) Delivery {
	return Delivery{
		Version: InboxVersion, DeliveryID: fmt.Sprintf("sweep-%d-%d-%s", now.UnixNano(), number, action),
		Event: "issues", Action: action, RepoID: registration.Entry.RepoID,
		RepositoryID: registration.Config.GitHub.RepositoryID, Repository: registration.Config.GitHub.Repo,
		IssueNumber: number, AcceptedAt: now,
	}
}

func (b *Broker) newSweepTimer(delay time.Duration) SweepTimer {
	if b.SweepTimers != nil {
		return b.SweepTimers.NewTimer(delay)
	}
	return systemSweepTimers{}.NewTimer(delay)
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
	b.deliveryMu.Lock()
	defer b.deliveryMu.Unlock()
	if _, err := os.Stat(b.receiptPath(delivery.DeliveryID)); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
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
	cleanup := time.NewTicker(time.Hour)
	defer cleanup.Stop()
	for {
		_ = b.replayOnce()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-cleanup.C:
			if err := b.compactReceipts(); err != nil {
				b.Logger.Printf("webhook receipt compaction failed")
			}
		}
	}
}

func (b *Broker) replayOnce() error {
	readDir := b.ReadDir
	if readDir == nil {
		readDir = os.ReadDir
	}
	entries, err := readDir(b.inboxDir())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			id := strings.TrimSuffix(entry.Name(), ".json")
			select {
			case b.work <- id:
			default:
			}
		}
	}
	return nil
}

func (b *Broker) route(id string) error {
	b.deliveryMu.Lock()
	defer b.deliveryMu.Unlock()
	path := b.inboxPath(id)
	if _, err := os.Stat(b.receiptPath(id)); err == nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var delivery Delivery
	if err := json.Unmarshal(data, &delivery); err != nil {
		return err
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
	if err := fsutil.WriteJSON(b.receiptPath(id), delivery, 0o600); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	syncDirectory(b.inboxDir())
	syncDirectory(b.receiptDir())
	b.mu.Lock()
	b.refreshDepthLocked()
	b.persistStatusLocked()
	b.mu.Unlock()
	return nil
}

func (b *Broker) inboxDir() string             { return filepath.Join(b.Root, "broker", "inbox") }
func (b *Broker) inboxPath(id string) string   { return filepath.Join(b.inboxDir(), id+".json") }
func (b *Broker) receiptDir() string           { return filepath.Join(b.Root, "broker", "receipts") }
func (b *Broker) receiptPath(id string) string { return filepath.Join(b.receiptDir(), id+".json") }
func (b *Broker) statusPath() string           { return filepath.Join(b.Root, "broker", "status.json") }

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
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			b.status.QueueDepth++
		}
	}
	b.status.UpdatedAt = b.Now()
}

func (b *Broker) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.mu.Lock()
			b.refreshDepthLocked()
			b.persistStatusLocked()
			b.mu.Unlock()
		}
	}
}

func (b *Broker) migrateRoutedInbox() error {
	entries, err := os.ReadDir(b.inboxDir())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(b.inboxDir(), entry.Name())
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var delivery Delivery
		if json.Unmarshal(data, &delivery) != nil || delivery.RoutedAt.IsZero() {
			continue
		}
		if err := fsutil.WriteJSON(filepath.Join(b.receiptDir(), entry.Name()), delivery, 0o600); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func (b *Broker) compactReceipts() error {
	b.deliveryMu.Lock()
	defer b.deliveryMu.Unlock()
	entries, err := os.ReadDir(b.receiptDir())
	if err != nil {
		return err
	}
	type receiptInfo struct {
		path string
		mod  time.Time
	}
	values := make([]receiptInfo, 0, len(entries))
	cutoff := b.Now().Add(-receiptRetention)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return statErr
		}
		path := filepath.Join(b.receiptDir(), entry.Name())
		if info.ModTime().Before(cutoff) {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return removeErr
			}
			continue
		}
		values = append(values, receiptInfo{path: path, mod: info.ModTime()})
	}
	if len(values) > receiptLimit {
		sort.Slice(values, func(i, j int) bool { return values[i].mod.Before(values[j].mod) })
		for _, value := range values[:len(values)-receiptLimit] {
			if err := os.Remove(value.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	syncDirectory(b.receiptDir())
	return nil
}

func syncDirectory(path string) {
	dir, err := os.Open(path)
	if err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
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
