# Applying project configuration

`mutagen project apply` applies changes to `mutagen.yml` without restarting
unchanged sessions:

```sh
mutagen project apply --dry-run   # show the plan
mutagen project apply
```

It starts the project if needed. It runs once and doesn't watch the file for
changes.


## What it changes

Sessions are matched by type and name within the project:

- New sessions are created, and removed ones are terminated.
- Sessions whose endpoints or merged configuration changed are recreated. This
  includes the `remote` and `remotePrologue` settings of synchronization
  sessions, although they only affect remote commands.
- Unchanged sessions keep their identifiers, synchronization history, scan
  caches, and paused state.
- Renaming a session removes the old session and creates a new one.

Docker environment variables such as `DOCKER_HOST` aren't compared. Retained
sessions keep the environment that they were created with, while new and
replaced pair sessions use the current one. In a [star](stars.md), the retained
edges determine the center's endpoint, including for new or replaced edges.


## Replaced sessions start over

Replaced synchronization sessions, like sessions created under a new name,
start with fresh history and scan caches. Without a record of the last synchronized state, they
can't tell which endpoint changed a file:

- Files that differ between the endpoints become conflicts in `two-way-safe`
  (the default) and `one-way-safe` modes, and are overwritten with alpha's
  version in `two-way-resolved` and `one-way-replica` modes.
- Files that exist on only one endpoint are copied to the other, which undoes
  deletions that weren't synchronized. Exceptions: `one-way-safe` leaves files
  that exist only on beta in place, and `one-way-replica` deletes them.


## How unsynchronized changes are protected

Before terminating any session, `apply` flushes the running synchronization
sessions that it replaces or removes, together with the running sessions that
share workspaces with them, including retained ones. Otherwise, the new
sessions could copy back files whose deletion hadn't reached every workspace.

- Sessions share a workspace if their endpoints are equal or nested on the same
  configured host. Host aliases and remote symbolic links aren't recognized.
- A flush only makes its own session's endpoints agree, so `apply` repeats the
  flushes until changes have propagated across connected sessions. A group
  of sessions connected this way is flushed in D+1 rounds, where D is the
  largest number of steps between two of them: two rounds for a star and k
  rounds for a chain of k sessions, or k rounds for k sessions whose
  workspaces form a cycle, since sessions that ignore a change or synchronize
  one way can force it the long way around. A session that shares no
  workspace is flushed once.
- If a flush fails, nothing is terminated or created. Fix the session and apply
  again. If its unsynchronized changes don't matter, terminate it with
  `mutagen sync terminate` and apply again. A retained session can instead be
  paused with `mutagen sync pause`, since it keeps its history.

What isn't protected:

- Changes that a flush leaves unsynchronized, such as those blocked by
  problems.
- Changes made while `apply` runs. Stop processes that modify synchronized
  files before replacing or renaming their sessions.
- Paused sessions, which are neither flushed nor resumed. Changes made since a
  session was paused may be overwritten (`two-way-resolved`,
  `one-way-replica`) or become conflicts (`two-way-safe`, `one-way-safe`) once
  its replacement is resumed, or once a session created under a new name
  synchronizes them. The plan notes such sessions: resume them before applying
  to synchronize their changes first.


## Options

- `-f path/to/project.yml` selects another project file. Relative endpoint
  paths are resolved against its directory.
- `--no-global-configuration` excludes global configuration. Use it
  consistently with `project start` and earlier `apply` invocations.
- `--paused` pauses newly created and replacement sessions without changing
  retained ones. Replacements also keep the previous session's paused state.


## Hooks and failures

- Creation hooks run only when starting a project. Applying changes to an
  existing project doesn't run lifecycle hooks, and startup hooks aren't
  replayed when retrying an existing project.
- `flushOnCreate` applies only to newly created, unpaused sessions.
- Changes aren't transactional: if an operation fails, the completed changes
  remain, and another `apply` reconciles the remaining sessions.
