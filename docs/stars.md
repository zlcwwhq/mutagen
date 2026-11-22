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
reaches every node over a single edge, while other nodes reach each other
through it, over two edges. Put it where you usually work, such as on the
system where your agents run.

Mutagen relays every session's data through the daemon, so every endpoint must
be reachable from the daemon, while endpoints needn't reach each other. When
the center isn't on the daemon's system, the daemon relays its edges as it
relays a pair whose endpoints are both remote. Commands on or from the center
cross a single edge, without intermediate copies, while a command from one
other node on another synchronizes the center's copy in between.


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

Each node, including the center, has two settings for commands:

- `remote` decides whether the node executes commands submitted from the
  nodes that reach it. `star.remote: true` enables execution on every node that
  doesn't set its own `remote`. It defaults to false, regardless of
  `sync.defaults.remote`.
- `remoteSubmit` decides whether commands may be submitted from the node to
  the nodes that it reaches. `star.remoteSubmit` sets it for every node that
  doesn't set its own, and defaults to true.

The center reaches every other node over the node's edge, and every other node
reaches the center over its own edge and the other nodes through the center. A
node runs commands on another if it may submit commands and the other executes
them. The center's settings don't affect commands that pass through it.

Any process running as your user on a node from which commands may be
submitted can run commands on every node that executes them. Only enable
execution on a node if you trust every node that may submit commands as much
as the node itself, and set `remoteSubmit: false` on the nodes that you don't.

```yaml
star:
  remote: true
  center: local
  nodes:
    local:
      endpoint: "."
      remote: false
    gpu:
      endpoint: "gpu:~/project"
    shared:
      endpoint: "shared:~/project"
      remote: false
    ci:
      endpoint: "ci:~/project"
      remoteSubmit: false
```

Here, the center can run commands on `gpu` and `ci`, but not on `shared`. `gpu`
and `shared` can run commands on `ci`, `shared` can run them on `gpu`, and no
node can run them on `shared` or on the center. Commands can't be submitted
from `ci`.

- A node that doesn't execute commands still synchronizes. `remote --nodes`
  lists it as unavailable, and operations on it fail with status 125.
- A node only connects for commands if commands may be submitted from it and a
  node that it reaches executes them. Otherwise, `remote` there finds no
  connected node (status 255).
- Each edge's remote command mode combines the `remote` settings of its ends,
  and its remote submission mode their `remoteSubmit` settings and whether
  they reach an executing node. `mutagen sync list -l` shows both: the center
  is alpha, and the other node is beta.
- Changing the center's `remote` replaces every edge, and changing its
  `remoteSubmit` replaces the edges of the nodes that execute commands.
  Changing another node's settings replaces its own edge, and, if the center
  doesn't execute commands, also the edges of the nodes for which it's the only
  node that executes them (see
  [Applying project configuration](project-apply.md)).
- Releases 0.5.0 and earlier don't load edges whose `remoteSubmit` settings
  keep a node from submitting commands that those releases would let it
  submit, rather than ignoring the restriction.
- In 0.4.0, a node's `remote` applied to its edge in both directions, and the
  center couldn't set it. Applying such a project again replaces the edges
  whose meaning changes: a node with `remote: false` can now run commands on
  the center unless the center sets `remote: false`, and a node with
  `remote: true` under `star.remote: false` can only run commands on the
  center if the center sets `remote: true`.
- In 0.5.0 and earlier, other nodes could only run commands on the center.
  With `remoteSubmit` defaulting to true, every node can now run commands on
  the other nodes that execute them: set `remoteSubmit: false` on the nodes
  that shouldn't, or `star.remoteSubmit: false` and enable it on those that
  should. Nodes that connected for commands before reach the other nodes once
  the daemon is upgraded. If the center doesn't execute commands, the other
  nodes connect once the project is applied again.

Select a node by name:

```sh
remote --nodes
remote --node gpu pytest -x                  # from the center or another node
remote --node server --timeout 2m make test  # from the center or another node
remote make test                             # on the center, from server or gpu
```

- From the center, `--node` selects any node, and can only be omitted if the
  star has a single other node.
- From any other node, `--node` selects the center or another node, and
  selects the center if omitted.

An operation on the center, or from it, crosses one edge, which is an ordinary
pair session, and behaves as it does for a pair (see
[Remote commands](remote.md)): the edge is flushed before and after commands,
conflicts produce warnings rather than preventing commands, and detached jobs,
`--no-flush`, `--flush`, `--pull`, `--diff`, `--ignores`, and conflict
resolution all work.

- An operation depends only on its own edge. Pausing, replacing, removing, or
  adding other nodes doesn't affect it.
- The edge must be connected for commands at both of its ends. While it isn't
  connected at the center, such as while it reconnects, the node is
  unavailable from the center, and operations on it from there fail with status
  255: retry once it reconnects.

An operation from another node on a node other than the center crosses both of
their edges, through the center:

- Before a command, the submitting node's edge is synchronized and then the
  destination's, so that changes reach the destination through the center.
  After it, the destination's edge is synchronized first, so that the
  command's changes come back. `--detach` and `--diff` synchronize as before a
  command, `--wait` as after one, and `--flush` without a command synchronizes
  the submitting node's edge again after both, so that changes arrive in both
  directions. Synchronization warnings name the edge that they come from.
- One-way edges flow from the center only, so the changes of a one-way
  submitting node don't reach the destination, and those of a one-way
  destination don't come back.
- Detached jobs, `--no-flush`, `--pull`, `--diff`, and `--ignores` work as
  they do on the center. Conflicts belong to each edge: list and resolve them
  from the center or from the destination, since operations on another node's
  edge fail with status 125.
- The operation depends on both edges, and on no other. Both must be connected
  at their nodes, but not at the center.

For every operation:

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

`remote --nodes` lists the nodes that the current node can reach, including
paused or disconnected ones: from the center, every node, and from another
node, itself, the center, and the other nodes, through the center. It marks
the current node and the center, and shows each node's host and root, the
mode of the edge to it, which, for a node reached through the center, is the
node's own edge, and its availability. `--nodes --json` gives
machine-readable output, in which each node's `path` lists the nodes from the
current node to it, including the center that it's reached through.

A node is available if it executes commands, as its remote setting permits,
and the edge to it is connected for commands: at both ends from the center,
and at the node from another node. Operations on a node that doesn't execute
commands fail with status 125. Listing nodes neither synchronizes files nor
executes commands, and being available doesn't mean being free of conflicts.
Terminating an edge only removes its node.


## Star problems

The daemon refuses star edges that disagree with the star's existing edges on
the center or its endpoint, and edges for a node that already has one. If the
sessions that it loads disagree on the center's endpoint anyway, then
`remote --nodes` reports the star problem, and new operations from or through
the center fail with status 125 until the star is repaired, such as by
terminating the edge that disagrees and applying the project again. Operations
already in progress, and the other nodes' operations on the center, keep using
their own edges.


## Workspace trees

Earlier releases declared workspace trees under `tree:`. `mutagen project
start` and `apply` refuse project files that still contain it. The
[0.2.0 release notes](https://github.com/zlcwwhq/mutagen/releases/tag/v0.19.0-zlcwwhq.0.2.0)
describe how to migrate a tree to a star.
