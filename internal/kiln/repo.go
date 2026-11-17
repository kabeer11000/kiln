package kiln

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// RepoConfig is a git repository checked out at /kiln/src before the command starts, so a
// stock image (node, python, golang...) can run a project without building an image.
type RepoConfig struct {
	URL   string `yaml:"url" json:"url"`
	Ref   string `yaml:"ref,omitempty" json:"ref,omitempty"`     // branch, tag or commit; default: the default branch
	Token string `yaml:"token,omitempty" json:"token,omitempty"` // secret NAME holding an https token, for private repos
}

// RepoInfo is what a run actually checked out. SHA makes the run reproducible.
type RepoInfo struct {
	URL string `json:"url"`
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

const repoDir = "/kiln/src"

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ResolveRepo pins the ref to a commit with `git ls-remote` (no clone), using this machine's
// git credentials, or the token from the environment if it is set here.
func ResolveRepo(ctx context.Context, c *RepoConfig) (*RepoInfo, error) {
	if c == nil {
		return nil, nil
	}
	ref := c.Ref
	if ref == "" {
		ref = "HEAD"
	}
	info := &RepoInfo{URL: c.URL, Ref: ref}
	if shaRe.MatchString(ref) {
		info.SHA = ref
		return info, nil
	}
	out, err := runCLI(ctx, "", "git", append(gitAuthArgs(c), "ls-remote", c.URL, ref, "refs/heads/"+ref, "refs/tags/"+ref, "refs/tags/"+ref+"^{}")...)
	if err != nil {
		return nil, &Error{Code: "repo_unreachable", Message: "could not read " + c.URL + ": " + err.Error(),
			Fix: "check `git ls-remote " + c.URL + "` works here; for private repos log in to git or set repo.token, or pin repo.ref to a full commit SHA"}
	}
	// Prefer an annotated tag's commit (^{}), then a branch, then anything else that matched.
	best, rank := "", 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		r := 1
		switch {
		case strings.HasSuffix(f[1], "^{}"):
			r = 3
		case f[1] == "refs/heads/"+ref:
			r = 2
		}
		if r > rank {
			best, rank = f[0], r
		}
	}
	if !shaRe.MatchString(best) {
		return nil, &Error{Code: "repo_ref_not_found", Message: "no branch or tag " + ref + " in " + c.URL,
			Fix: "set repo.ref to an existing branch, tag, or full commit SHA"}
	}
	info.SHA = best
	return info, nil
}

// gitAuthArgs sends the token as an HTTP header, so it never lands in a URL, a remote or .git/config.
func gitAuthArgs(c *RepoConfig) []string {
	if c == nil || c.Token == "" {
		return nil
	}
	tok := os.Getenv(c.Token)
	if tok == "" {
		return nil
	}
	return []string{"-c", "http.extraHeader=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok))}
}

// cloneLocal checks out info.SHA into dir with this machine's git (docker/podman backends).
func cloneLocal(ctx context.Context, c *RepoConfig, info *RepoInfo, dir string) error {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	git := func(args ...string) error {
		// safe.directory: kiln made this checkout; drives without ownership info (FAT/exFAT, some
		// network shares) would otherwise make git refuse it as "dubious".
		cmd := exec.CommandContext(ctx, "git", append(append(gitAuthArgs(c), "-c", "safe.directory=*", "-C", dir), args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return &Error{Code: "repo_clone_failed", Message: "git " + args[0] + ": " + strings.TrimSpace(stderr.String()),
				Fix: "check `git ls-remote " + info.URL + "` works here"}
		}
		return nil
	}
	if err := git("init", "-q"); err != nil {
		return err
	}
	// Shallow fetch of the exact commit; servers that refuse fetching by SHA get a full fetch.
	if git("fetch", "-q", "--depth", "1", info.URL, info.SHA) != nil {
		if err := git("fetch", "-q", info.URL, "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*"); err != nil {
			return err
		}
	}
	return git("checkout", "-q", info.SHA)
}
