# PwnMesh containers

Build both images from the repository root:

```sh
docker build -t pwnmesh:dev .
docker build -f container/Dockerfile -t pwnmesh-worker:dev .
```

See [the development guide](../docs/development.md) for the shared build and
validation entry points, and [supported formats](../docs/formats.md) before
migrating an existing installation.

The current server uses SQLite schema `2` and `pwnmesh_*` SQL object names. Stop
the old Server and Dispatcher before the first upgraded startup; a supported
schema `1` database is renamed transactionally on open. Preserve its installation
ID and workspace volumes. This database migration does not rename containers,
volumes or workspace files, and older binaries must not share the upgraded database.

Worker containers are owned by a persistent installation ID stored in the server
database, plus the configured dispatcher namespace and project ID. Restarting
against the same database reuses that identity. A different database cannot adopt
or clean up its containers. Only one dispatcher may be active across all
namespaces in an installation, keeping each project's steps in one workspace.
The namespace remains part of container ownership and lease operations.

New worker containers store `/workspace` in a named Docker volume scoped to that
installation, namespace, and project. The volume retains the image's initial
workspace contents, including `.git`. `completed_action: remove` removes the
container while preserving this workspace; starting another round reuses it.
Deleting the project removes its owned workspace volume once Docker confirms it
is unused, even if the container has already been removed. Failed volume cleanup
is retried through the normal dispatcher cleanup loop.

Workspace volumes also retain their original image ID, network, and capabilities.
Changes to that runtime require deliberate migration instead of silently reusing
old evidence in a different environment. Shared-network aliases are resolved to
immutable container IDs. Unexpected volume ownership, storage options, or
workspace mounts are rejected rather than adopted or deleted.

Existing owned containers without a workspace volume remain usable. For these
containers, `completed_action: remove` stops and preserves the container so its
writable-layer evidence survives; explicit project deletion still removes it.
PwnMesh does not automatically attach volumes or migrate these workspaces.

Legacy containers named `<namespace>-dispatch-<project>` are preserved and block
replacement creation. Before upgrading an existing project, stop the old
dispatcher, back up its `/workspace`, and migrate the workspace deliberately;
PwnMesh does not automatically relabel or delete containers with unknown ownership.
Database backups preserve installation identity: do not run a restored clone as
an independent deployment against the same Docker daemon and namespace.

The first dispatcher also binds the installation to its container namespace.
Changing that namespace is rejected even after lease expiry or release, because
existing workspaces and orphan cleanup still belong to the original namespace.
After deliberately stopping and migrating every owned container and workspace
volume, an administrator may clear `pwnmesh_dispatchers` in the offline database
to establish a new binding.

Native `serve` and `dispatch` default to
`$HOME/.local/share/pwnmesh/model-settings.json`, independent of the SQLite path.
Run both under the same user, or set the same absolute `PWNMESH_MODEL_CONFIG_PATH`
for both processes. An existing database-adjacent `model-settings.json` outside
that default is preserved; `serve` reports its path and requires this explicit
setting before startup. Containers continue to use the path configured by Compose.
