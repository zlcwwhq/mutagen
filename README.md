# Mutagen

Mutagen is a new kind of remote development tool that enables your existing
local tools to work with code in remote environments like cloud servers and
containers. It does this by providing high-performance real-time
[file synchronization](https://mutagen.io/documentation/synchronization) and
flexible [network forwarding](https://mutagen.io/documentation/forwarding).
It supports synchronization and forwarding between
[local systems](https://mutagen.io/documentation/transports/local),
[SSH-accessible locations](https://mutagen.io/documentation/transports/ssh),
and [Docker containers](https://mutagen.io/documentation/transports/docker).


## Getting started

The best way to understand Mutagen and its features is to read the
[Overview](https://mutagen.io/documentation/introduction) and
[Getting started](https://mutagen.io/documentation/introduction/getting-started)
guides. You can find information about all of Mutagen's features in the
[documentation](https://mutagen.io/documentation).


## Applying project configuration

Use `mutagen project apply` to apply changes to `mutagen.yml` without restarting
unchanged sessions:

```sh
mutagen project apply --dry-run
mutagen project apply
```

The command starts the project if needed and matches existing sessions by type
and name within that project. It creates new sessions, terminates removed
sessions, and recreates sessions whose endpoints or merged configurations have
changed. Unchanged sessions retain their identifiers, synchronization history,
scan caches, and paused state. Renaming a session removes the old session and
creates a new one; replaced synchronization sessions start with fresh history
and scan caches. Docker environment variables such as `DOCKER_HOST` are not
compared: retained sessions keep the environment they were created with, while
new and replaced sessions use the current environment.

Use `-f path/to/project.yml` for another project file. Relative endpoint paths
are resolved against its directory. Global configuration is included unless
`--no-global-configuration` is specified; use this flag consistently with
`project start` and previous `apply` invocations. `--paused` pauses newly created
and replacement sessions without changing retained sessions. Replacements also
preserve the previous session's paused state.

Creation hooks run only when starting a project. Applying changes to an existing
project does not run lifecycle hooks. `flushOnCreate` applies only to newly
created, unpaused sessions. Changes are not transactional: if an operation fails,
completed changes remain and another `apply` reconciles the remaining sessions.
Startup hooks are not replayed when retrying an existing project. The command
runs once; it does not watch the YAML file for changes.


## Installation

You can find
[installation instructions](https://mutagen.io/documentation/introduction/installation)
in the [Mutagen documentation](https://mutagen.io/documentation).


## Community

The [Mutagen Community Slack Workspace](https://mutagen.io/slack) is the place
to go for discussion, questions, and ideas.

For updates about the project and its releases, you can
[follow Mutagen on Twitter](https://twitter.com/mutagen_io).


## Status

Mutagen is built and tested on Windows, macOS, and Linux, and it's available for
[many more platforms](https://github.com/mutagen-io/mutagen/releases/latest).

| Tests                               | Report card                         |
| :---------------------------------: | :---------------------------------: |
| [![Tests][tests-badge]][tests-link] | [![Report card][rc-badge]][rc-link] |

[tests-badge]: https://github.com/mutagen-io/mutagen/workflows/CI/badge.svg "Test status"
[tests-link]: https://github.com/mutagen-io/mutagen/actions "Test status"
[rc-badge]: https://goreportcard.com/badge/github.com/mutagen-io/mutagen "Report card status"
[rc-link]: https://goreportcard.com/report/github.com/mutagen-io/mutagen "Report card status"


## License

For license information, please see the [LICENSE](LICENSE) file.


## Contributing

If you'd like to contribute to Mutagen, please see the
[contribution documentation](CONTRIBUTING.md).


## Security

Mutagen takes security very seriously. If you believe you have found a security
issue with Mutagen, please practice responsible disclosure practices and send an
email directly to [security@docker.com](mailto:security@docker.com) instead of
opening a GitHub issue. For more information, please see the
[security documentation](SECURITY.md).


## Versioning and support

Mutagen uses [semantic versioning](https://semver.org/) for tracking releases.
Before Mutagen reaches version 1.0, each minor release series will be supported
for one month after the first release in the next minor release series (e.g.
v0.10.x will be supported until one month after the release of v0.11.0).

The builds for each minor release series are pinned to the same Go minor release
and dependency versions used for developing that series (though patch releases
will be incorporated if they contain security fixes).

We reserve the right to break or bend these rules if necessary for the sake of
security, e.g. discontinuing support for a minor release to which a security fix
cannot be backported or upgrading Go minor versions for a release series to
incorporate important security fixes. Features marked as experimental may also
see breakage when necessary, though we will endeavor to keep this to a minimum.


## Building

Please see the [build instructions](BUILDING.md).


## External projects

Users have built a number of cool projects to extend and integrate Mutagen into
their workflows:

- [**Mutagen Helper**](https://github.com/gfi-centre-ouest/mutagen-helper) is a
  tool that makes the orchestration of synchronization sessions even easier by
  letting you define sessions with configuration files that live inside your
  codebase. Thanks to [**@Toilal**](https://github.com/Toilal)!
- [**docker-magento-mutagen**](https://github.com/mage2click/docker-magento-mutagen)
  (from [Mage2click](https://mage2.click/)) is a Docker-based development
  environment for Magento that uses Mutagen for file synchronization. Thanks to
  [**@u-maxx**](https://github.com/u-maxx) and
  [**@shkoliar**](https://github.com/shkoliar)!
- [**MutagenMon**](https://github.com/rualark/MutagenMon) is a Python-based GUI
  for monitoring Mutagen sessions. Thanks to
  [**@rualark**](https://github.com/rualark)!
- [**mutagenmon**](https://github.com/andrewmed/mutagenmon) is an alternative
  (Go-based) native GUI for Mac, for monitoring Mutagen sessions, provides apple
  notarized builds.


## Unrelated projects

Please note that this project is unrelated to the
[Mutagen](https://github.com/quodlibet/mutagen) Python module used for handling
audio metadata.
