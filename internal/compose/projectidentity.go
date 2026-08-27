package compose

import (
	"context"
	"fmt"
	"os"
)

// Project identity resolution.
//
// A DIRECTORY IS NOT A PROJECT. `docker compose` derives a project name from
// the working directory only when nothing else names one, and every project
// created with `-p` or COMPOSE_PROJECT_NAME breaks that assumption while still
// reporting the same ConfigDir. The TUI has always carried the real name — its
// picker rows come straight out of `docker compose ls` — while a composer built
// from a bare directory carried none, so the SAME project was addressed under
// two identities and keyed to two different state files: a TUI deploy recorded
// its digests under sha256(dir+NUL+name) and a `cdeploy rollback -C <dir>` read
// sha256(dir), silently restoring a different deploy's images.
//
// ResolveProject closes that split by making the identity canonical BEFORE it
// reaches any key: it looks the composer up in `docker compose ls` once and
// stamps the project's real name, its own compose file set, and the directory
// docker reports for it. `--project-name` becomes an OVERRIDE of that lookup
// rather than the only way to obtain a named identity, so the TUI fast track,
// a grouped drill-in and every CLI verb converge on one identity — and one
// state-file key — for one project.

// resolveIdentity is the transport-independent core of ResolveProject: given
// the project list docker reported, it picks the row a (dir, name) pair
// addresses and reports whether that directory holds more than one project.
//
// A NAMED lookup matches on the name alone — the name IS the identity, and the
// directory only says where the files live. An UNNAMED lookup matches on the
// directory, and only when exactly one project lives there: two `-p` projects
// sharing a directory are two identities, and picking one would address the
// wrong container set.
//
// dir must already be normalized by the caller's norm func (localProjectDir for
// a local host, remoteProjectDir for a remote one), which is also applied to
// each reported ConfigDir so equivalent spellings collapse.
func resolveIdentity(projects []Project, dir, name string, norm func(string) string) (proj Project, ok, dirShared bool) {
	inDir := projectsInDir(projects, dir, norm)
	dirShared = len(inDir) > 1

	if name != "" {
		for _, p := range projects {
			if p.Unmanaged {
				continue
			}
			if p.Name == name {
				return p, true, dirShared
			}
		}
		return Project{}, false, dirShared
	}

	if len(inDir) != 1 {
		return Project{}, false, dirShared
	}
	return inDir[0], true, dirShared
}

// projectsInDir returns every reported project whose config directory is dir.
// The synthetic unmanaged row and any project docker reports with no
// config_files label are skipped: neither has a directory to match on.
func projectsInDir(projects []Project, dir string, norm func(string) string) []Project {
	if dir == "" {
		return nil
	}
	var out []Project
	for _, p := range projects {
		if p.Unmanaged || p.ConfigDir == "" {
			continue
		}
		if norm(p.ConfigDir) == dir {
			out = append(out, p)
		}
	}
	return out
}

// errProjectNotFound is returned when an explicitly named project is not among
// the projects docker reports. Named as a sentinel so callers can distinguish a
// user error from a transport failure.
var errProjectNotFound = fmt.Errorf("project not found")

// notFoundError renders the refusal for an explicitly named project docker does
// not report. `--project-name` selects an EXISTING project: cdeploy has no `-f`
// flag, so proceeding would recreate the named project from whatever compose
// file the directory happens to hold — another project's definitions under the
// requested label, which is destructive and silent.
func notFoundError(name string) error {
	return fmt.Errorf("%w: %q is not among the projects docker reports (docker compose ls -a); --project-name selects an existing project", errProjectNotFound, name)
}

// ResolveProject resolves this composer's project identity against the projects
// docker reports, and is idempotent — the lookup costs one `docker compose ls`
// per composer, cached in projectResolved.
//
// Precedence:
//  1. ProjectName already set (the `--project-name` flag): STRICT. The row is
//     looked up by name; a lookup that fails to complete, or completes without
//     finding it, is an error. Silently falling back to auto-discovered files
//     under that label is the destructive case this refuses.
//  2. COMPOSE_PROJECT_NAME in the environment: SOFT. The local docker CLI
//     inherits os.Environ, so this name is what compose itself would use;
//     adopting it keeps cdeploy from overriding the user's own selection with a
//     directory lookup. A name docker does not report is kept as-is (compose
//     would create it), not refused.
//  3. Neither: SOFT. The directory is looked up, and only an unambiguous match
//     is adopted. A failure to list leaves the composer directory-addressed —
//     exactly the pre-resolution behavior — because a host without a usable
//     `docker compose ls` must still be able to deploy.
//
// A resolved row contributes its NAME, its own compose FILE SET (unless the
// caller already supplied one) and its config DIRECTORY: docker is the
// authority on where a project lives, and adopting its spelling is what makes
// two composers built from two different spellings of one directory key to one
// state file.
//
// The `.env` file is deliberately NOT parsed for COMPOSE_PROJECT_NAME. It would
// mean replicating compose's interpolation rules to guess an identity, and the
// only case it changes is a directory whose single reported project is not the
// one `.env` names.
func (c *Compose) ResolveProject(ctx context.Context) error {
	if c.projectResolved {
		return nil
	}

	explicit := c.ProjectName != ""
	if !explicit {
		if env := os.Getenv("COMPOSE_PROJECT_NAME"); env != "" {
			c.ProjectName = env
		}
	}
	if c.ProjectDir == "" && c.ProjectName == "" {
		c.projectResolved = true
		return nil
	}

	projects, err := c.ListProjects(ctx)
	if err != nil {
		if explicit {
			return fmt.Errorf("resolving project %q: %w", c.ProjectName, err)
		}
		return nil
	}

	proj, ok, dirShared := resolveIdentity(projects, localProjectDir(c.ProjectDir), c.ProjectName, localProjectDir)
	c.legacyStateBlocked = dirShared
	c.projectResolved = true
	if !ok {
		if explicit {
			return notFoundError(c.ProjectName)
		}
		return nil
	}

	c.ProjectName = proj.Name
	if len(c.ComposeFiles) == 0 {
		c.ComposeFiles = proj.ConfigFiles
	}
	if proj.ConfigDir != "" {
		c.ProjectDir = proj.ConfigDir
	}
	return nil
}

// ResolveProject is the remote twin of Compose.ResolveProject; see that doc
// comment for the precedence rules. The one difference is COMPOSE_PROJECT_NAME:
// SSH does not carry the local environment to the remote shell, so a local
// value says nothing about which project a remote command would address and is
// not consulted.
func (r *RemoteCompose) ResolveProject(ctx context.Context) error {
	if r.projectResolved {
		return nil
	}
	if r.ProjectDir == "" && r.ProjectName == "" {
		r.projectResolved = true
		return nil
	}

	explicit := r.ProjectName != ""
	projects, err := r.ListProjects(ctx)
	if err != nil {
		if explicit {
			return fmt.Errorf("resolving project %q: %w", r.ProjectName, err)
		}
		return nil
	}

	proj, ok, dirShared := resolveIdentity(projects, remoteProjectDir(r.ProjectDir), r.ProjectName, remoteProjectDir)
	r.legacyStateBlocked = dirShared
	r.projectResolved = true
	if !ok {
		if explicit {
			return notFoundError(r.ProjectName)
		}
		return nil
	}

	r.ProjectName = proj.Name
	if len(r.ComposeFiles) == 0 {
		r.ComposeFiles = proj.ConfigFiles
	}
	if proj.ConfigDir != "" {
		r.ProjectDir = proj.ConfigDir
	}
	return nil
}
