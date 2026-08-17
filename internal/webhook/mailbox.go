package webhook

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
)

func MailboxDir(repoStateDir string) string {
	return filepath.Join(repoStateDir, "webhook-mailbox")
}

// ReadMailbox returns every durable delivery. The scheduler coalesces targets
// before remote reads; AckMailbox then removes the complete acted-on batch.
func ReadMailbox(repoStateDir string) ([]Delivery, error) {
	dir := MailboxDir(repoStateDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]Delivery, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var delivery Delivery
		if err := json.Unmarshal(data, &delivery); err != nil {
			return nil, err
		}
		result = append(result, delivery)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AcceptedAt.Before(result[j].AcceptedAt) })
	return result, nil
}

func AckMailbox(repoStateDir string, deliveries []Delivery) error {
	dir := MailboxDir(repoStateDir)
	for _, delivery := range deliveries {
		if err := os.Remove(filepath.Join(dir, delivery.DeliveryID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func EnqueueMailbox(repoStateDir string, delivery Delivery) error {
	dir := MailboxDir(repoStateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return fsutil.WriteJSON(filepath.Join(dir, delivery.DeliveryID+".json"), delivery, 0o600)
}
