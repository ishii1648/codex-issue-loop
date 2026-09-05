package delivery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
)

const slotManifestVersion = 1

type SlotManifest struct {
	Version        int       `json:"version"`
	ReleaseVersion string    `json:"release_version"`
	Commit         string    `json:"commit"`
	ArtifactSHA256 string    `json:"artifact_sha256"`
	Binary         string    `json:"binary"`
	StagedAt       time.Time `json:"staged_at"`
}

func SlotRef(l layout.Layout, version, commit, digest string) AssignmentRef {
	dir := filepath.Join(l.DeliverySlotsDir(), safeName(version)+"-"+digest)
	return AssignmentRef{Version: version, Commit: commit, ArtifactSHA256: digest, Slot: filepath.Join(dir, "agent-loop")}
}

func StageSlot(l layout.Layout, ref AssignmentRef, source string) error {
	if err := validateAssignmentRef(ref); err != nil {
		return err
	}
	want := SlotRef(l, ref.Version, ref.Commit, ref.ArtifactSHA256)
	if filepath.Clean(want.Slot) != filepath.Clean(ref.Slot) {
		return errors.New("assignment slot is not the canonical version and digest path")
	}
	if digest, err := fileDigest(source); err != nil {
		return err
	} else if digest != ref.ArtifactSHA256 {
		return errors.New("slot source digest does not match the assignment")
	}
	if err := ensurePrivateDirectory(l.DeliverySlotsDir()); err != nil {
		return err
	}
	dir := filepath.Dir(ref.Slot)
	if _, err := os.Lstat(dir); err == nil {
		return VerifySlot(ref)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.MkdirTemp(l.DeliverySlotsDir(), ".slot-")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := os.Chmod(tmp, 0o700); err != nil {
		return err
	}
	binary, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFile(filepath.Join(tmp, "agent-loop"), binary, 0o700); err != nil {
		return err
	}
	manifest := SlotManifest{Version: slotManifestVersion, ReleaseVersion: ref.Version, Commit: ref.Commit, ArtifactSHA256: ref.ArtifactSHA256, Binary: "agent-loop", StagedAt: time.Now().UTC()}
	if err := fsutil.WriteJSON(filepath.Join(tmp, "slot.json"), manifest, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	keep = true
	return VerifySlot(ref)
}

func VerifySlot(ref AssignmentRef) error {
	if err := validateAssignmentRef(ref); err != nil {
		return err
	}
	dir := filepath.Dir(ref.Slot)
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() || dirInfo.Mode().Perm()&0o077 != 0 {
		return errors.New("assignment slot directory must be an owner-only regular directory")
	}
	info, err := os.Lstat(ref.Slot)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o100 == 0 {
		return errors.New("assignment slot binary must be an owner-only executable regular file")
	}
	manifestPath := filepath.Join(dir, "slot.json")
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return err
	}
	if manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode().Perm()&0o077 != 0 {
		return errors.New("assignment slot manifest must be an owner-only regular file")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest SlotManifest
	if err := decodeStrictJSON(data, &manifest); err != nil {
		return fmt.Errorf("decode slot manifest: %w", err)
	}
	if manifest.Version != slotManifestVersion || manifest.ReleaseVersion != ref.Version || manifest.Commit != ref.Commit || manifest.ArtifactSHA256 != ref.ArtifactSHA256 || manifest.Binary != filepath.Base(ref.Slot) {
		return errors.New("assignment slot manifest does not match the assignment")
	}
	digest, err := fileDigest(ref.Slot)
	if err != nil {
		return err
	}
	if digest != ref.ArtifactSHA256 {
		return errors.New("assignment slot binary digest mismatch")
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("delivery path is not a regular directory: %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}
