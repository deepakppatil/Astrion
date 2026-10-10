package supervisor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/execapi"
	"github.com/deepakppatil/astrion/internal/procgroup"
)

var safeName = regexp.MustCompile(`[^A-Za-z0-9._\-]+`)

// prepareRepositories clones each repository at its requested revision,
// reports the resolved commit (REPO-006), and creates a working branch when
// the command may modify code. Any failure identifies the repository and
// fails the job before the agent starts (SRS 13.2).
func (s *Supervisor) prepareRepositories(ctx context.Context) ([]domain.JobRepository, error) {
	var out []domain.JobRepository
	used := map[string]bool{}
	for _, r := range s.spec.Repositories {
		name := r.CheckoutPath
		if name == "" {
			name = r.RepositoryID
		}
		name = strings.Trim(safeName.ReplaceAllString(name, "_"), "._")
		if name == "" || used[name] {
			name = r.RepositoryID
		}
		used[name] = true
		dest := filepath.Join(s.reposDir, name)
		s.logs.add("supervisor", "info", fmt.Sprintf("checking out repository %s at %s", r.RepositoryID, r.Revision))
		commit, err := s.checkout(ctx, r, dest)
		if err != nil {
			return nil, fmt.Errorf("checkout of repository %s failed: %v", r.RepositoryID, err)
		}
		jr := domain.JobRepository{RepositoryID: r.RepositoryID, URL: r.URL, RequestedRevision: r.Revision,
			ResolvedCommit: commit, CheckoutPath: dest}
		if r.WorkingBranch != "" {
			if _, err := s.git(ctx, dest, nil, "checkout", "-q", "-b", r.WorkingBranch); err != nil {
				return nil, fmt.Errorf("create working branch for repository %s: %v", r.RepositoryID, err)
			}
			jr.WorkingBranch = r.WorkingBranch
		}
		if err := s.cfg.API.Repository(ctx, execapi.RepositoryReport{RepositoryID: r.RepositoryID,
			ResolvedCommit: commit, CheckoutPath: dest, WorkingBranch: jr.WorkingBranch}); err != nil {
			return nil, fmt.Errorf("report repository %s: %v", r.RepositoryID, err)
		}
		out = append(out, jr)
	}
	return out, nil
}

func (s *Supervisor) checkout(ctx context.Context, r execapi.RepoSpec, dest string) (string, error) {
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if r.CredentialFile != "" {
		credPath := filepath.Join(s.cfg.SecretsDir, filepath.Base(r.CredentialFile))
		user := "x-access-token"
		if r.Provider == "gitlab" {
			user = "oauth2"
		}
		// GIT_ASKPASS reads the token from the secret file at use time so the
		// token never appears in argv, the remote URL, or .git/config.
		askpass := filepath.Join(s.stateDir, "askpass.sh")
		script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n  Username*) echo %q ;;\n  *) cat %q ;;\nesac\n", user, credPath)
		if err := os.WriteFile(askpass, []byte(script), 0o700); err != nil {
			return "", err
		}
		env = append(env, "GIT_ASKPASS="+askpass)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", err
	}
	rev := r.Revision
	if rev == "" {
		rev = "HEAD"
	}
	steps := [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", r.URL},
		{"fetch", "-q", "--depth", "1", "origin", rev},
		{"checkout", "-q", "--detach", "FETCH_HEAD"},
	}
	for _, args := range steps {
		if _, err := s.git(ctx, dest, env, args...); err != nil {
			return "", err
		}
	}
	commit, err := s.git(ctx, dest, env, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(commit), nil
}

func (s *Supervisor) git(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.cfg.Git, args...)
	cmd.Dir = dir
	cmd.Env = append(append(procgroup.BaseEnv(s.homeDir, os.TempDir()),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Astrion", "GIT_AUTHOR_EMAIL=astrion@localhost",
		"GIT_COMMITTER_NAME=Astrion", "GIT_COMMITTER_EMAIL=astrion@localhost"), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, s.red.String(msg))
	}
	return stdout.String(), nil
}

// collectRepositoryChanges records a diff artifact and a local commit for
// each writable repository with changes. Pushing and change-request creation
// are separate, policy-gated steps (REPO-004/005).
func (s *Supervisor) collectRepositoryChanges(ctx context.Context, repos []domain.JobRepository) error {
	for _, r := range repos {
		if r.WorkingBranch == "" {
			continue
		}
		if _, err := s.git(ctx, r.CheckoutPath, nil, "add", "-A"); err != nil {
			return err
		}
		diff, err := s.git(ctx, r.CheckoutPath, nil, "diff", "--cached", "--binary", "HEAD")
		if err != nil {
			return err
		}
		if strings.TrimSpace(diff) == "" {
			s.logs.add("supervisor", "info", "no changes in repository "+r.RepositoryID)
			continue
		}
		if _, err := s.upload(ctx, execapi.ArtifactUpload{UploadID: "diff-" + r.RepositoryID, Type: "diff",
			Name: r.RepositoryID + ".diff", ContentType: "text/x-diff", Classification: domain.ArtifactUser,
			RepositoryID: r.RepositoryID, Step: "collect-changes"}, []byte(diff)); err != nil {
			return err
		}
		msg := fmt.Sprintf("astrion: %s (job %s)", s.spec.Command, s.spec.JobID)
		if _, err := s.git(ctx, r.CheckoutPath, nil, "commit", "-q", "-m", msg); err != nil {
			return err
		}
		head, err := s.git(ctx, r.CheckoutPath, nil, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if err := s.cfg.API.Repository(ctx, execapi.RepositoryReport{RepositoryID: r.RepositoryID,
			FinalCommit: strings.TrimSpace(head), WorkingBranch: r.WorkingBranch}); err != nil {
			return err
		}
	}
	return nil
}

// createArchive builds a tar.gz of regular files and directories under dir.
// Symlinks and special files are skipped.
func createArchive(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// extractArchive safely extracts a tar.gz into dir, rejecting absolute
// paths, traversal, links, and archives larger than maxBytes.
func extractArchive(data []byte, dir string, maxBytes int64) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(hdr.Name)
		if filepath.IsAbs(name) {
			return fmt.Errorf("archive entry %q is absolute", hdr.Name)
		}
		target := filepath.Join(dir, name)
		if !within(dir, target) {
			return fmt.Errorf("archive entry %q escapes the destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > maxBytes {
				return errors.New("archive exceeds size limit")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, io.LimitReader(tr, hdr.Size)); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive entry %q has unsupported type %c", hdr.Name, hdr.Typeflag)
		}
	}
}
