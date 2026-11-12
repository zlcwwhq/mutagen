# Mutagen

This is a fork of [Mutagen](https://github.com/mutagen-io/mutagen) for running
commands on the other side of a synchronization session. Keep your editor and
coding agents on your own machine, and run tests, builds, or training on the
server or container that holds the synchronized copy:

```sh
cd ~/project/service
remote pytest -x               # runs in ~/project/service on the server
```

Mutagen provides real-time file synchronization and network forwarding between
local systems, SSH-accessible locations, and Docker containers. The
[Mutagen documentation](https://mutagen.io/documentation) covers everything
that this fork doesn't change.


## What the fork adds

- [**Remote commands**](docs/remote.md): `remote` runs a command in the
  corresponding directory on the other endpoint, synchronizing before and after
  it, and forwards its streams and exit status. It also runs detached jobs,
  copies ignored output back, and resolves conflicts.
- [**Git mode**](docs/git-mode.md): `.gitignore` files decide what's
  synchronized, `.git` never is, and submodules follow the commit checked out
  on the other endpoint.
- [**Star workspaces**](docs/stars.md): a project joins one center workspace to
  other workspaces, each through its own session.
- [**`mutagen project apply`**](docs/project-apply.md) applies changes to
  `mutagen.yml` without restarting unchanged sessions.
- [**Agent skills**](#agent-skills) teach coding agents to use `remote` and to
  set Mutagen up.


## Installation

Releases are on the [releases page](https://github.com/zlcwwhq/mutagen/releases).
Each platform's bundle contains `mutagen` and `remote`. The agents that
`mutagen` installs on endpoints are a separate asset for all platforms,
`mutagen-agents_v<version>.tar.gz`. Save it as `mutagen-agents.tar.gz` next to
`mutagen`, or in `../libexec` if `mutagen` is in a `bin` directory, and replace
it whenever you upgrade: `mutagen` refuses agents from another release.

Only the system that runs the daemon needs an installation. On Linux, macOS,
and FreeBSD,
[`install.sh`](https://github.com/zlcwwhq/mutagen-remote-skill/blob/main/skills/setup/scripts/install.sh)
from the `setup` agent skill (see below) downloads, verifies, and installs or
upgrades to the latest release.


## Quick start

Enable remote commands for a session in `mutagen.yml`:

```yaml
sync:
  code:
    alpha: "."
    beta: "server:~/project"
    remote: true
```

Start it, then run commands from anywhere inside the project:

```sh
mutagen project start
remote --nodes                                     # alpha and beta
remote go test ./...
id=$(remote --detach --log train.log python train.py)
remote --wait "$id" --follow
```

In a Git repository, add `git: enabled` so that `.gitignore` decides what's
synchronized (see [Git mode](docs/git-mode.md)). To activate an environment
such as conda before each command, use a
[prologue](docs/remote.md#prologues).


## Agent skills

The `mutagen-remote` plugin from
[mutagen-remote-skill](https://github.com/zlcwwhq/mutagen-remote-skill) holds
two skills, which agents see as `mutagen-remote:remote` and
`mutagen-remote:setup`. The `remote` skill teaches coding agents to use the
`remote` executable. The `setup` skill installs or upgrades this fork, starts
the daemon, writes `mutagen.yml`, starts the sessions, and makes `remote`
available on the endpoints. Install the plugin on each system where agents
submit commands or set up Mutagen.

Claude Code:

```
/plugin marketplace add zlcwwhq/mutagen-remote-skill
/plugin install mutagen-remote@mutagen-remote-skill
```

Codex:

```sh
codex plugin marketplace add zlcwwhq/mutagen-remote-skill
codex plugin add mutagen-remote@mutagen-remote-skill
```

Codex's default sandbox blocks `remote`'s connection to the daemon. The
skill's README covers the sandbox setting, updates, and an example setup. This
repository pins the matching version of the skills as the
`skills/mutagen-remote` submodule.


## Development

- [BUILDING.md](BUILDING.md): building and regenerating protobufs.
- [CONTRIBUTING.md](CONTRIBUTING.md): code style, tests, and the DCO.
- [SECURITY.md](SECURITY.md): reporting security issues.
- [LICENSE](LICENSE): license information.
