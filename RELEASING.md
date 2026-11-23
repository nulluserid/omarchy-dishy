# Releasing Dishy

The short loop: release tree → PR → verified merge → tag → deploy → marketplace
update. Anything pushed to `main` is what the marketplace's install command
gives users (it clones mutable HEAD), so `main` stays shippable.

## 1. Land the work

Feature branches via PR, reviewed before push. Docs-only fixes may land
directly on `main`.

## 2. Release commit

Prepare the release tree on the feature branch before merge:

1. Bump `version` in `manifest.json`.
2. Rebuild the committed binary for **every versioned release**, including
   QML-only releases: `backend/build.sh`. The default stamp is the manifest
   version, not the previous Git tag. The exact compiler in `backend/go.mod`
   is required; if needed select it with `GOTOOLCHAIN=go1.27.4` (use the
   version in go.mod when it changes). Builds no longer run `go mod tidy` or
   silently change dependencies. Commit deliberate dependency changes separately
   from running the build.
3. When a compiler or linked dependency changes, review its upstream license and
   notices, update the reviewed inventory in `backend/check-licenses.sh`, then run
   `bash backend/check-licenses.sh --write` with the exact build compiler. Commit
   `backend/THIRD_PARTY_NOTICES.txt` alongside the binary, including upstream
   copyright/NOTICE texts (not just dependency names). Retain these notices when
   distributing the executable separately from the plugin checkout.
4. Run the gate below on the final release tree, pass exact-diff review, then
   commit/push and merge the PR only after its CI gate passes.
5. On `main`, run the gate again at the merged SHA, tag annotated `vX.Y.Z`,
   push the tag, and publish the GitHub release with notes. The manifest,
   executable stamp, and tag must identify the same version.

### Mandatory pre-submission gate

Build tools: exact Go from `backend/go.mod`, `jq`, `protoc` (CI pins 36.1),
`protoc-gen-go@v1.36.12`, `protoc-gen-go-grpc@v1.6.2`, and
`govulncheck@v1.9.0`. Install Go tools explicitly with `go install` at those
versions. Run:

```bash
bash backend/verify-release.sh
omarchy plugin validate .
```

The script regenerates ignored protobuf stubs, verifies module checksums and
tidiness, checks formatting, runs race tests/vet, compares a clean deterministic
rebuild to the **committed** executable, checks its compiler/static-build metadata
and manifest stamp, verifies bundled third-party texts against a reviewed linked
component/toolchain inventory and the exact upstream files, and scans **both source and binary** against the live Go
vulnerability database. Scanner errors and findings fail the gate; text mode is
intentional because JSON/SARIF output does not fail on findings. An offline
`GOVULNDB=file:///...` snapshot is for isolated local verification only, not a
substitute for CI's live advisory check before submission.

The Release gate workflow runs on PRs, main pushes, manual requests, and a daily
schedule. Watch scheduled failures: a previously clean binary can acquire newly
published vulnerabilities without any source change. A clean scan covers known
Go advisories, not all possible bugs.

Before asking a maintainer to check again, record these checks in the release/PR:

- Source boundaries: hostile dish arrays/floats/maps, control inputs, egress
  responses, disk/permission upgrades, installer/service destination, QML consumers.
- Artifact boundaries: actual linked compiler and module versions, current
  advisories, build settings, reproducibility, and manifest/binary/tag consistency.
- Real surface: isolated daemon smoke test; plugin validation and local UI smoke
  when QML changes — the demo kit (`backend/demo/`) allows a local UI smoke and
  fresh screenshots while off Starlink. Name anything not exercised rather than
  calling it covered: a demo run stands in for the dish, the egress provider
  and the notifier, so it exercises none of the real ones, nor the live
  service.
- Evidence identity: **full final main SHA**, binary SHA-256, tool/scanner versions,
  successful CI URL, and an independent review of the exact outgoing diff.

Why these are separate: the v1.5.1 source audit missed GO-2026-6809 in the
statically linked Go runtime, along with dependency advisories, and its release
left the manifest at 1.5.0. Reviewing source and printing build metadata did not
check that metadata against current advisories or the shipped release identity.

The binary is deterministic (`CGO_ENABLED=0 -trimpath -buildvcs=false`);
verify a checkout with `go version -m backend/bin/starlinkd` and
`backend/bin/starlinkd -version`. Restart the daemon if `backend/` or
`deploy/starlinkd.service` changed.

## 3. Deploy locally

```
omarchy plugin update --yes
cd ~/.config/omarchy/plugins/io.github.nulluserid.dishy
./install.sh        # restarts the canonical plugin binary
```

If any file other than the entry point changed — `Panel.qml`, `Model.js`,
`Sparkline.qml`, `ObsMap.qml` (everything except `BarWidget.qml`) — also
restart the shell (`omarchy-restart-shell`): the plugin file watcher does
**not** invalidate the QML engine cache for non-entrypoint files, so the
new panel only appears after a shell restart. Verify against the live
state files in `~/.local/state/starlinkd/` (`$XDG_STATE_HOME/starlinkd` when
that variable is set; `$XDG_RUNTIME_DIR/starlinkd` always resolves to the
daemon's state dir).

For the 1.7.0 upgrade the restart is not conditional: run `./install.sh`,
then restart the shell. Until the new widget loads it sends no heartbeat, and
the new unit's daemon exits 15 minutes after `install.sh` restarted it. The
daemon republishes `$XDG_RUNTIME_DIR/starlinkd` at every start.

Plugin QML must qualify the palette singleton — `import qs.Commons as
Commons`, then `Commons.Color.*` — because Qt 6.12 ships its own `Color`
type that would otherwise shadow it.

## 4. Marketplace update

The listing (omarchy-plugin-marketplace issue for Dishy) is pinned to an
exact commit. Once `main` moves past it, the store shows **Update
unverified** until the update is verified — normal and cosmetic, but close
it promptly:

1. Open the marketplace's **Plugin verification** issue form
   (`verify-plugin.yml`) and select **Verify and publish a newer upstream
   commit**.
2. Provide the plugin ID (`io.github.nulluserid.dishy`), the repository root
   URL, and the full 40-char SHA of the new `main` HEAD.
3. A bot runs the security baseline against that exact commit; a maintainer
   applies `approved-and-verified` and the listing snapshot advances.

The listing's automated baseline must stay at its three capabilities — the
bundled binary, service management and the installer. The scanner reads
`.sh .bash .zsh .fish .qml .js .mjs .cjs .py .rb .pl .lua .service .sudoers
.toml .yaml .yml .desktop` files, the root README, every executable file,
extensionless files, and anything named install, installer, setup or uninstall
(including `backend/test-install.sh`); it does not read `.go` files, `.md`
files other than the root README, or `.github/`. In what it reads, never put
a package-manager command name or a privilege-escalation word outside a
negated phrase, and never in an `echo`: a whole-tree run over-reports, so
predict the bot with the scanner's real scope.

While the initial submission is still under review, reply on that issue with
the same evidence and the final SHA instead of filing a duplicate submission.
Do not name a source-only fix commit as the shipped release: the rebuilt binary
must already be in the SHA being submitted.

Keep `main` clean between releases — every commit on it is what
`omarchy plugin add` hands users until the next verification lands.

## 5. First submission (already done)

Filed as `[Plugin]: Dishy`
(omarchy-plugin-marketplace#13443), snapshot commit `344b12b` (v1.5.0).
Initial review workflow: bot baseline → maintainer `approved-and-verified`
label → published with a verified snapshot.
