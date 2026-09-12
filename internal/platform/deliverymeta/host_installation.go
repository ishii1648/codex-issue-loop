package deliverymeta

import (
	"errors"
	"path/filepath"

	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
)

type HostInstallation struct {
	Format      int           `json:"format"`
	Version     string        `json:"version"`
	Commit      string        `json:"commit"`
	Digest      string        `json:"binary_sha256"`
	SkillDigest string        `json:"skill_sha256"`
	Bootstrap   AssignmentRef `json:"bootstrap_runtime"`
	Previous    string        `json:"previous,omitempty"`
}

func ReadHostInstallation(l layout.Layout) (HostInstallation, error) {
	var installed HostInstallation
	data, err := fsutil.ReadPrivateRegular(filepath.Join(l.Root, "host-install.json"), "host install manifest")
	if err != nil {
		return installed, err
	}
	if err := fsutil.DecodeStrictJSON(data, &installed); err != nil {
		return installed, err
	}
	if installed.Format != 1 || !ValidDigest(installed.Digest) || !ValidDigest(installed.SkillDigest) {
		return installed, errors.New("invalid host install manifest")
	}
	if installed.Bootstrap != SlotRef(l, installed.Bootstrap.Version, installed.Bootstrap.Commit, installed.Bootstrap.ArtifactSHA256) {
		return installed, errors.New("bootstrap runtime is outside trusted slots")
	}
	if err := VerifySlot(installed.Bootstrap); err != nil {
		return installed, err
	}
	return installed, nil
}
