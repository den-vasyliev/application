# Releasing

Releases are managed via `make bump-patch` and `make bump-minor`. Each target updates the `VERSION` file, commits, creates a git tag, and pushes to origin.

## Bump patch (e.g. v1.1.8 → v1.1.9)

```bash
make bump-patch
```

## Bump minor (e.g. v1.1.x → v1.2.0)

```bash
make bump-minor
```

Both targets:
1. Update `VERSION` (increments `app_patch` or `app_minor`, resets patch to 0 for minor)
2. Commit: `release: bump patch to vX.Y.Z`
3. Tag: `vX.Y.Z`
4. Push commit and tag to `origin master`

CI then builds and pushes the multi-arch image (`linux/amd64`, `linux/arm64`) to:
```
ghcr.io/den-vasyliev/application:<tag>
ghcr.io/den-vasyliev/application:latest  # on default branch
```
