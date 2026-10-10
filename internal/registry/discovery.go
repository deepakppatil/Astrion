package registry

import (
	"bytes"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/deepakppatil/astrion/internal/domain"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// Discoverer inspects framework sources and proposes commands. Discovered
// commands are always returned as PENDING_REVIEW: framework- or
// repository-controlled definitions are untrusted until approved (SRS 9).
type Discoverer interface {
	Name() string
	Discover(frameworkID string, src fs.FS) ([]Command, error)
}

// ManifestDiscoverer reads an "astrion-commands.json" manifest containing a
// list of command definitions (same schema as declared commands).
type ManifestDiscoverer struct{}

func (ManifestDiscoverer) Name() string { return "manifest" }

func (ManifestDiscoverer) Discover(frameworkID string, src fs.FS) ([]Command, error) {
	b, err := fs.ReadFile(src, "astrion-commands.json")
	if err != nil {
		return nil, err
	}
	var cmds []Command
	if err := strictUnmarshal(b, &cmds); err != nil {
		return nil, err
	}
	return finalizeDiscovered(frameworkID, cmds)
}

// MarkdownDirDiscoverer recognises frameworks that ship one Markdown file per
// slash command in a directory (e.g. ".claude/commands/<name>.md"). It only
// proposes the command identity; phase and permissions must be assigned by an
// administrator during review, so commands default to a restrictive profile.
type MarkdownDirDiscoverer struct {
	Dir          string       // e.g. ".claude/commands"
	DefaultPhase domain.Phase // phase to propose; reviewers may change it
}

func (d MarkdownDirDiscoverer) Name() string { return "markdown-dir" }

func (d MarkdownDirDiscoverer) Discover(frameworkID string, src fs.FS) ([]Command, error) {
	var cmds []Command
	err := fs.WalkDir(src, d.Dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		name := strings.TrimSuffix(path.Base(p), ".md")
		phase := d.DefaultPhase
		if phase == "" {
			phase = domain.PhasePlan
		}
		cmds = append(cmds, Command{
			ID:               domain.NewID(domain.PrefixCommand),
			Canonical:        "/" + name, // casing preserved exactly as in the framework
			DisplayName:      name,
			Description:      "Discovered from " + p + " (pending review)",
			Phase:            phase,
			RepositoryAccess: RepoRead,
			SideEffects:      []SideEffect{EffectNone},
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return finalizeDiscovered(frameworkID, cmds)
}

func finalizeDiscovered(frameworkID string, cmds []Command) ([]Command, error) {
	out := make([]Command, 0, len(cmds))
	for _, c := range cmds {
		c.FrameworkID = frameworkID
		c = c.WithDefaults()
		c.Source = SourceDiscovered
		c.Status = StatusPendingReview
		if c.ID == "" {
			c.ID = domain.NewID(domain.PrefixCommand)
		}
		if err := c.Validate(); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}
