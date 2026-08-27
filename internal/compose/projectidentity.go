package compose

import (
	"path"
	"regexp"
	"strings"
)

// Project identity, and the rule that keeps every entry point keying ONE state
// file per project.
//
// A composer's identity is whatever its CALLER supplied — the `--project-name`
// flag, or the name on the picker row the TUI drilled into. It is NEVER
// re-derived from `docker compose ls`.
//
// That lookup is what a previous round used, and it is unsafe by construction:
// `ls` enumerates projects by scanning CONTAINER LABELS, so a project whose
// containers no longer exist is not reported at all — and the deploy pipeline
// itself removes them (stop → rm -f → pull → create → start). A snapshot key
// derived from the lookup therefore changed identity exactly when a failed
// deploy needed it most: the rollback the snapshot exists for could no longer
// find its own state file. THE STORAGE KEY MUST NOT DEPEND ON STATE THE
// OPERATION BEING SNAPSHOTTED CAN DESTROY.
//
// canonicalStateName folds the two spellings of ONE project onto one key with
// no lookup at all. An UNNAMED compose invocation is not identity-free: it
// addresses the project compose derives from the directory's base name. So a
// composer naming that default project and a composer naming nothing are the
// same project, and both key the dir-only hash every cdeploy release has
// written. A project that is NOT the directory default (`-p blue`) keys its
// own file, so two `-p` projects sharing one directory never read each other's
// digests.
//
// Worked through: the TUI fast track (no name), a grouped drill-in on row
// "app" in /srv/app, and `cdeploy rollback -C /srv/app` all address project
// app and all key sha256("/srv/app"); `cdeploy deploy -C /srv/app -p blue` and
// a drill-in on row "blue" both key sha256("/srv/app\x00blue").
func canonicalStateName(dir, name string) string {
	if name == "" || name == composeDefaultProjectName(dir) {
		return ""
	}
	return name
}

// composeProjectNameRune matches the characters compose keeps when it derives a
// project name from a directory.
var composeProjectNameRune = regexp.MustCompile(`[a-z0-9_-]`)

// composeDefaultProjectName reproduces compose's own rule for the project an
// unnamed invocation addresses: the directory's base name, lower-cased, with
// every character outside [a-z0-9_-] dropped and any leading `_`/`-` trimmed
// (compose-go's NormalizeProjectName). This is not a guess about which project
// the user means — it is the definition of the one compose would use.
//
// dir must already be normalized (localProjectDir / remoteProjectDir). POSIX
// path semantics are used unconditionally: this tool is unix-only per the SSH /
// ControlMaster design, and a remote directory is a POSIX path regardless of
// where cdeploy runs.
//
// Compose consults `-p`, then COMPOSE_PROJECT_NAME, then a top-level `name:`
// key, then `.env`, before falling back to this rule. cdeploy reads NONE of
// those: the environment is invisible over SSH, so honoring it locally would
// split the local and remote keys for one project, and parsing `.env` or the
// compose file means replicating compose's interpolation. When one of them
// names a project this derivation does not predict, the named and unnamed
// spellings key two files and a rollback reports "no snapshot" — a safe
// failure, never a wrong image.
func composeDefaultProjectName(dir string) string {
	if dir == "" {
		return ""
	}
	base := strings.ToLower(path.Base(dir))
	base = strings.Join(composeProjectNameRune.FindAllString(base, -1), "")
	return strings.TrimLeft(base, "_-")
}

// stateName is the canonical project name this composer keys its state file
// under, and the name stamped into the file. Key and stamp come from this one
// function so they can never disagree.
func (c *Compose) stateName() string {
	return canonicalStateName(localProjectDir(c.ProjectDir), c.ProjectName)
}

// stateName is the remote twin of Compose.stateName.
func (r *RemoteCompose) stateName() string {
	return canonicalStateName(remoteProjectDir(r.ProjectDir), r.ProjectName)
}

// composeOverrideFiles are the default override names compose loads ALONGSIDE
// the main file during auto-discovery. They belong in the auto-discoverable set
// even though HasComposeFile does not probe for them: a project created with an
// override reports both files, and pinning that pair would be indistinguishable
// from pinning a hand-picked `-f` set.
var composeOverrideFiles = []string{
	"compose.override.yml",
	"compose.override.yaml",
	"docker-compose.override.yml",
	"docker-compose.override.yaml",
}

// PinComposeFiles decides whether a composer built from a picker row should
// carry that row's reported compose files as `-f` pairs.
//
// `docker compose ls` reports config_files from a LABEL docker stamped when the
// containers were CREATED, and `-f` DISABLES compose's file auto-discovery.
// Pinning a label that names only default-discoverable files therefore froze
// the file set at container-creation time: a docker-compose.override.yml added
// afterwards was silently ignored, and `up --no-start` re-stamped the same
// one-file label, so the pin could never heal itself.
//
// So the set is pinned ONLY when auto-discovery could not reproduce it — a file
// outside the project directory, or one whose name compose does not look for
// (`-f prod.yml`). That is exactly the case a bare directory gets wrong:
// without the pin, a project created from prod.yml was recreated from a sibling
// docker-compose.yml under the right label. Returning nil means auto-discover,
// which is byte-identical to the pre-ComposeFiles argv.
func PinComposeFiles(configDir string, files []string) []string {
	if len(files) == 0 || configDir == "" {
		return files
	}
	for _, f := range files {
		if !autoDiscoverable(configDir, f) {
			return files
		}
	}
	return nil
}

// autoDiscoverable reports whether compose's own file discovery in dir would
// find f: it must live directly in dir and carry a default name.
func autoDiscoverable(dir, f string) bool {
	if f == "" || path.Dir(f) != path.Clean(dir) {
		return false
	}
	base := path.Base(f)
	for _, name := range composeFiles {
		if base == name {
			return true
		}
	}
	for _, name := range composeOverrideFiles {
		if base == name {
			return true
		}
	}
	return false
}
