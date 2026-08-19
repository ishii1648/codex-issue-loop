package github

// PullRequestHeadRepository decodes the repository identity returned by
// gh pr list. gh currently exposes the repository name and owner as separate
// JSON fields, while older fixtures may provide nameWithOwner directly.
type PullRequestHeadRepository struct {
	HeadRepository *struct {
		NameWithOwner string `json:"nameWithOwner"`
		Name          string `json:"name"`
	} `json:"headRepository"`
	HeadRepositoryOwner *struct {
		Login string `json:"login"`
	} `json:"headRepositoryOwner"`
}

// FullName returns owner/name only when the available repository identity is
// complete. An incomplete current gh shape remains empty so callers fail
// closed when validating a Pull Request.
func (r PullRequestHeadRepository) FullName() string {
	if r.HeadRepository == nil {
		return ""
	}
	if r.HeadRepository.NameWithOwner != "" {
		return r.HeadRepository.NameWithOwner
	}
	if r.HeadRepositoryOwner == nil || r.HeadRepositoryOwner.Login == "" || r.HeadRepository.Name == "" {
		return ""
	}
	return r.HeadRepositoryOwner.Login + "/" + r.HeadRepository.Name
}
