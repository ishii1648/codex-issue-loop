package github

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

func (c CLI) IsCommitAncestor(ctx context.Context, cfg config.Config, ancestor, head string) (bool, error) {
	for _, sha := range []string{ancestor, head} {
		if decoded, err := hex.DecodeString(sha); err != nil || len(decoded) != 20 {
			return false, fmt.Errorf("invalid commit identity")
		}
	}
	if ancestor == head {
		return true, nil
	}
	path := c.Path
	if path == "" {
		path = "gh"
	}
	output, err := exec.CommandContext(ctx, path, "api", "repos/"+cfg.GitHub.Repo+"/compare/"+ancestor+"..."+head).CombinedOutput()
	if err != nil {
		return false, c.commandError(ctx, path, "verify published commit ancestry", err, output)
	}
	var comparison struct {
		Status string `json:"status"`
		Base   struct {
			SHA string `json:"sha"`
		} `json:"base_commit"`
		MergeBase struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := json.Unmarshal(output, &comparison); err != nil {
		return false, err
	}
	return comparison.Status == "ahead" && comparison.Base.SHA == ancestor && comparison.MergeBase.SHA == ancestor, nil
}
