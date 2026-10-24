---
name: release
description: Prepares a HyperHand release commit — CHANGELOG section, the required checks and the commit — and stops before the tag. Use when the user asks to release, cut a version or publish; pushing the v* tag publishes a GitHub Release and needs the user's explicit release authorization.
argument-hint: [version]
---

# Release preparation

A release is a commit on the current branch that turns `## [Unreleased]` in `CHANGELOG.md` into the version's section. The version is not stored in any file: the workflow stamps it from the tag. Pushing the `v<version>` tag is the publication ([Building: Releasing](../../../docs/building.md#releasing), `.github/workflows/release.yml`); this skill prepares everything up to the commit and does nothing to the tag unless the user has explicitly authorized the release.

## Steps

1. **Clean tree.** `git status --porcelain` is empty; every change that belongs to the release is already committed. Do not mix release and feature changes in one commit.
2. **Version.** Use the version the user named. Without one, propose one by Semantic Versioning (before 1.0: a breaking change or a new tool raises the minor version, fixes alone the patch version), say why, and wait for the user's choice.
3. **CHANGELOG.** Rename `## [Unreleased]` to `## [<version>] - <today, YYYY-MM-DD>`. Entries describe what a user or AI caller notices, name every needed `vm_update_agent` and migration, and leave out test and process details; no empty `###` sections. Sections of tagged versions are never edited. Afterwards `grep -n Unreleased README.md CHANGELOG.md` prints nothing. The workflow uses this section as the release notes; without it they are only `HyperHand <version>`.
4. **Checks.** `scripts/test-windows.sh` and `python3 -m unittest discover -s client` pass, and both executables build with `GOOS=windows`. Optionally check a candidate package as in [Manual verification: Release package](../../../docs/manual-verification.md#release-package).
5. **Commit** on the current branch: `CHANGELOG: <version>`. No `Co-Authored-By` or other generation marker. Push only when the user asked for a push.
6. **Tag.** Only with the user's explicit release authorization in this conversation: `git tag v<version>` and `git push origin v<version>`. The workflow tests, builds `hyperhand-<version>-windows-amd64.zip` (both executables, `README.md`, `CHANGELOG.md`) and the single-file installer `hyperhand-<version>-windows-amd64.exe` (agent embedded), and publishes the GitHub Release. Without that authorization, report that the commit is ready and that tagging publishes.
7. **After publishing**, when the user asks: put both executables of the released zip into `build/dev-install/`, install them without UAC with `build/dev-install/hyperhand.exe dev-install --no-build`, run `vm_update_agent` on each test VM (`dev-install` skill, steps 2 and 3) and check that `vm_status` reports the released version.
