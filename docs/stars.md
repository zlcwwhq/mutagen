# Star workspaces

A project can declare one star of directory workspaces: a center node joined to
every other node by its own synchronization session, all managed by one daemon.

```yaml
star:
  remote: true
  sync:
    ignore:
      paths: ["/mutagen.yml.lock", ".venv/", "__pycache__/"]
  center: local
  nodes:
    local:
      endpoint: "."
    server:
      endpoint: "server:~/project"
    gpu:
      endpoint: "gpu:~/project"
      mode: one-way-safe
      remotePrologue: "~/.config/gpu-prologue.sh"
```

Apply it with `mutagen project apply --dry-run`, then `mutagen project apply`
(see [Applying project configuration](project-apply.md)).


## Contents

- [Nodes and edges](#nodes-and-edges)
- [Choosing the center](#choosing-the-center)
- [Synchronization settings](#synchronization-settings)
- [Overlapping workspaces](#overlapping-workspaces)
- [Remote commands](#remote-commands)
- [Listing nodes](#listing-nodes)
- [Star problems](#star-problems)
- [Workspace trees](#workspace-trees)


## Nodes and edges

- Each node is one directory on one system. `center` is required and names one
  of the nodes, and there must be at least one other node.
- Node names start with an ASCII letter, contain letters, digits, or dashes,
  end with a letter or digit, and have at most 63 characters. `defaults` is
  reserved.
- The star expands into one ordinary synchronization session for each node
  other than the center, named `star-<node>`, whose alpha is the center and
  whose beta is the node. `mutagen project list` shows them.
- Each session records its star, center, and node in a dedicated field rather
  than in labels, so its membership survives daemon restarts and can't be set
  or changed with `mutagen sync create --label`. `mutagen sync list -l` shows
  it.
- Applying configuration changes keeps the history of unchanged edges. New or
  replaced edges reuse the center's endpoint from the star's retained edges,
  including its captured Docker environment, so that all edges still identify
  the same center.


## Choosing the center

Any node can be the center, with any type of endpoint, but only the center
reaches every node. Put it where you usually work, such as on the system where
your agents run.

Mutagen relays every session's data through the daemon, so every endpoint must
be reachable from the daemon, while endpoints needn't reach each other. When
the center isn't on the daemon's system, the daemon relays its edges as it
relays a pair whose endpoints are both remote. Either way, each command crosses
a single edge, without intermediate copies.


## Synchronization settings

- Settings under `star.sync` extend the global and project synchronization
  defaults, including ignores and [Git mode](git-mode.md), and apply to every
  edge.
- A node's `mode` sets the synchronization mode of its own edge
  (`two-way-safe`, `two-way-resolved`, `one-way-safe`, or `one-way-replica`),
  taking precedence over `star.sync.mode` and the defaults. Edges use
  `two-way-safe` if nothing selects a mode. `mode` can't be set on the center.
- The center is alpha on every edge, so it wins conflicts in
  `two-way-resolved` mode, and one-way modes flow from the center to the node.
  Changes made on a one-way node, including files that commands write there,
  don't reach the center.
- Pair-specific `configurationAlpha` and `configurationBeta` defaults don't
  apply.
- Configure prologues with each node's `remotePrologue`, including the
  center's. `remotePrologue` in global or project synchronization defaults
  doesn't apply to star nodes.


## Overlapping workspaces

The nodes of a star must not overlap one another, including the center. The
daemon refuses star edges whose nodes would overlap, and `mutagen project
start` and `apply` refuse such declarations, as well as sessions in the same
project file that overlap star workspaces.

Sessions created outside the project aren't checked. Overlapping them behaves
like overlapping pairs: the most specific workspace that contains a directory
is the source for `remote`, workspaces with the same root are ambiguous, and
changes propagate through both sessions.

Detection compares configured hosts and paths and can't see through host
aliases or remote symbolic links, so don't use them to give one workspace
several identities.


## Remote commands

`star.remote: true` permits commands between the center and every other node.
It defaults to false, regardless of `sync.defaults.remote`. Select a node by
name:

```sh
remote --nodes
remote --node gpu pytest -x                  # from the center
remote --node server --timeout 2m make test  # from the center
remote make test                             # on the center, from server or gpu
```

- From the center, `--node` selects any node, and can only be omitted if the
  star has a single other node.
- From any other node, the center is the only destination, so `--node` can be
  omitted. Selecting another node fails with status 125: stars have no
  multi-hop routes, so run such commands from the center.

Every operation crosses exactly one edge, which is an ordinary pair session,
and behaves as it does for a pair (see [Remote commands](remote.md)): the edge
is flushed before and after commands, conflicts produce warnings rather than
preventing commands, and detached jobs, `--no-flush`, `--flush`, `--pull`,
`--diff`, `--ignores`, and conflict resolution all work.

- An operation depends only on its own edge. Pausing, replacing, removing, or
  adding other nodes doesn't affect it.
- The edge must be connected for commands at both of its ends. While it isn't
  connected at the center, such as while it reconnects, the node is
  unavailable, and operations on it from the center fail with status 255:
  retry once it reconnects.
- Commands run in the corresponding relative directory; `--cwd` overrides it.
- Repeat `--node` for later operations on the same node, such as `--wait` and
  `--kill` for a detached job. The commands that `remote` suggests include it.
- `MUTAGEN_REMOTE_NODE` names the executing node. Use it in prologues shared by
  several nodes instead of `MUTAGEN_REMOTE_ENDPOINT`, since the center is alpha
  on every edge and every other node is beta.

`remote --conflicts --json` reports an `edges` array with a single entry, which
names the edge's `alpha` and `beta` nodes and the listing's `source` and
`target`, and contains the `listing`. `remote --help nodes` summarizes target
selection.


## Listing nodes

`remote --nodes` lists the nodes that the current node can reach: from the
center, every node, including paused or disconnected ones, and from another
node, itself and the center. It marks the current node and the center, and
shows each node's host and root, the mode of the edge to it, and its
availability. `--nodes --json` gives machine-readable output.

A node is available if it executes commands, as the remote setting of its edge
permits, and that edge is connected for commands at both ends. Operations on a
node that doesn't execute commands fail with status 125. Listing nodes neither
synchronizes files nor executes commands, and being available doesn't mean
being free of conflicts. Terminating an edge only removes its node.


## Star problems

The daemon refuses star edges that disagree with the star's existing edges on
the center or its endpoint, and edges for a node that already has one. If the
sessions that it loads disagree on the center's endpoint anyway, then
`remote --nodes` reports the star problem, and new operations from the center
fail with status 125 until the star is repaired, such as by terminating the
edge that disagrees and applying the project again. Operations already in
progress, and the other nodes, keep using their own edges.


## Workspace trees

Earlier releases declared workspace trees under `tree:`. `mutagen project
start` and `apply` refuse project files that still contain it. The
[0.2.0 release notes](https://github.com/zlcwwhq/mutagen/releases/tag/v0.19.0-zlcwwhq.0.2.0)
describe how to migrate a tree to a star.
