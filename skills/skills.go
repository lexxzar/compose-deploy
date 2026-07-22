// Package skills embeds the cdeploy Agent Skill (SKILL.md, per the agentskills.io
// open standard) so it can be installed into AI coding-agent skill directories by
// the `cdeploy skill` subcommand. The embedded content is version-locked to the
// binary via go:embed — the file on disk under skills/cdeploy/ is the single
// source of truth for both this package and external installers
// (npx skills / gh skill), which read the repo-root skills/cdeploy/SKILL.md.
package skills

import "embed"

// FS holds the embedded skill directory tree (currently cdeploy/SKILL.md).
// The directory layout mirrors what installers write to disk.
//
//go:embed cdeploy
var FS embed.FS

// canonicalSkillPath is the path of the canonical SKILL.md within FS.
const canonicalSkillPath = "cdeploy/SKILL.md"

// CanonicalSkill returns the raw, unstamped bytes of the embedded SKILL.md.
// These are the canonical bytes: install-time rendering injects a metadata
// stamp before writing, and verification strips it back to compare against
// exactly these bytes. The go:embed directive guarantees the file exists, so a
// read error here is a build/packaging fault and panics rather than returning an
// error callers would have to thread everywhere.
func CanonicalSkill() []byte {
	data, err := FS.ReadFile(canonicalSkillPath)
	if err != nil {
		panic("skills: embedded " + canonicalSkillPath + " missing: " + err.Error())
	}
	return data
}
