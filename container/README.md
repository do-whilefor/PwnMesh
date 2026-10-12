```
docker build . -t cairn-worker-container
```

Worker containers are owned by a persistent installation ID stored in the server
database, plus the configured dispatcher namespace and project ID. Restarting
against the same database reuses that identity. A different database cannot adopt
or clean up its containers. Only one dispatcher may be active across all
namespaces in an installation, keeping each project's steps in one workspace.
The namespace remains part of container ownership and lease operations.

Legacy containers named `<namespace>-dispatch-<project>` are preserved and block
replacement creation. Before upgrading an existing project, stop the old
dispatcher, back up its `/workspace`, and migrate the workspace deliberately;
PwnMesh does not automatically relabel or delete containers with unknown ownership.
Database backups preserve installation identity: do not run a restored clone as
an independent deployment against the same Docker daemon and namespace.

The first dispatcher also binds the installation to its container namespace.
Changing that namespace is rejected even after lease expiry or release, because
existing workspaces and orphan cleanup still belong to the original namespace.
After deliberately stopping and migrating every owned container, an administrator
may clear `pwnmesh_dispatchers` in the offline database to establish a new binding.
