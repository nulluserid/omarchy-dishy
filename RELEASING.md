# Releasing Dishy

The short loop: PR → merge → release commit → tag → deploy → marketplace
update. Anything pushed to `main` is what the marketplace's install command
gives users (it clones mutable HEAD), so `main` stays shippable.

## 1. Land the work

Feature branches via PR, reviewed before push. Docs-only fixes may land
directly on `main`.

## 2. Release commit

On `main`:

1. Bump `version` in `manifest.json`.
2. If `backend/` changed: rebuild the committed binary, version-stamped —
   `VERSION=vX.Y.Z backend/build.sh` (needs `go` + `protoc`). QML/docs-only
   releases leave the binary alone.
3. Commit as `release: vX.Y.Z`, tag annotated `vX.Y.Z`, push `main` + tag,
   publish the GitHub release with notes.

The binary is deterministic (`CGO_ENABLED=0 -trimpath -buildvcs=false`);
verify a checkout with `go version -m backend/bin/starlinkd` and
`backend/bin/starlinkd -version`.

## 3. Deploy locally

```
omarchy plugin update --yes
./install.sh        # restarts starlinkd (needed whenever backend/ changed)
```

If `Panel.qml` / `BarWidget.qml` / `Model.js` changed, also restart the
shell (`omarchy-restart-shell`): the plugin file watcher does **not**
invalidate the QML engine cache for non-entrypoint files, so the new panel
only appears after a shell restart. Verify against the live state files in
`~/.local/state/starlinkd/`.

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

Keep `main` clean between releases — every commit on it is what
`omarchy plugin add` hands users until the next verification lands.

## 5. First submission (already done)

Filed as `[Plugin]: Dishy`
(omarchy-plugin-marketplace#13443), snapshot commit `344b12b` (v1.5.0).
Initial review workflow: bot baseline → maintainer `approved-and-verified`
label → published with a verified snapshot.
