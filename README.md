# servercrafter

servercrafter builds Minecraft server directories (Paper, Folia, Velocity…) from declarative YAML
templates: server jar, plugins, worlds, configuration files and patches.
The same binary serves as the container entrypoint: it verifies the installed files and injects
secrets into the configuration at startup.

```console
$ servercrafter build lobby -o server
INFO Installed server.jar (1.21.4-232)
INFO Installed plugins/LuckPerms.jar (5.5.91)
INFO Installed plugins/ViaVersion.jar (5.12.2)
INFO Built lobby into server: 4 artifacts in 1.56s
```

- [Quick start](#quick-start)
- [The project file](#the-project-file-servercrafteryaml)
- [Templates](#templates)
- [Inheritance](#inheritance)
- [Artifacts and downloads](#artifacts-and-downloads)
- [Overlays and patches](#overlays-and-patches)
- [Build, manifest and diff](#build-manifest-and-diff)
- [Bootstrap](#bootstrap)
- [Commands](#commands)

## Quick start

```sh
go install github.com/discowzombie/servercrafter/cmd/servercrafter@latest

cd examples
servercrafter list                # buildable templates and their inheritance chain
servercrafter validate --resolve  # checks everything, resolves versions without downloading anything
servercrafter build lobby -o ../out/lobby
```

The [`examples/`](examples) directory contains a complete project: a partial shared by all Paper
servers, a lobby, its dev variant and a Velocity proxy.

## The project file (`servercrafter.yaml`)

It declares where the templates live and where artifacts come from. It is looked up in the current
directory (use `--project`/`-p` or `SERVERCRAFTER_PROJECT` for another path). Without it,
servercrafter still works: templates are read from the current directory and only public
downloads (`url`, `papermc`, `modrinth`, `github`, `luckperms`) are available.

```yaml
# yaml-language-server: $schema=schemas/project.schema.json
version: 1
templatesDir: templates          # relative to the project file

credentials:
  gitlab:   { type: header, name: PRIVATE-TOKEN, value: { env: GITLAB_TOKEN } }
  nexus:    { type: basic, username: { env: NEXUS_USER }, password: { file: /run/secrets/nexus } }
  github:   { type: bearer, token: { env: GITHUB_TOKEN } }
  maps-ssh: { type: ssh, privateKey: { file: ~/.ssh/id_ed25519 }, passphrase: { env: KEY_PASS } }
  scw:      { type: s3, accessKey: { env: S3_ACCESS_KEY }, secretKey: { env: S3_SECRET_KEY }, region: fr-par }

sources:
  static: { type: local, directory: static }
  dev:    { type: local, directory: static/dev }
  cdn:    { type: http, baseUrl: https://cdn.example.com/, credentials: nexus }
  nexus:  { type: nexus, baseUrl: https://nexus.example.com, repository: maven-releases, credentials: nexus }
  gitlab: { type: gitlab, baseUrl: https://gitlab.example.com, group: minecraft, credentials: gitlab }
  maps:   { type: s3, baseUrl: https://s3.fr-par.scw.cloud/maps/, credentials: scw }
  worlds: { type: git, repository: git@gitlab.example.com:mc/worlds.git, ref: main, credentials: maps-ssh }
```

### Credentials

A credential says **where to read** a secret, never the secret itself: `{ env: VAR }` or
`{ file: path }` (Docker/Kubernetes secrets; relative to the project, `~` accepted). A plaintext
secret is rejected; only a `basic` username, which is not a secret, may be written as
`{ value: name }`.

| type     | fields                                     | usage                                        |
|----------|--------------------------------------------|----------------------------------------------|
| `basic`  | `username`, `password`                     | http, nexus, gitlab, git over https          |
| `header` | `name`, `value`                            | `PRIVATE-TOKEN`, `JOB-TOKEN`, `Deploy-Token`… |
| `bearer` | `token`                                    | `Authorization: Bearer …` (GitHub…)          |
| `ssh`    | `privateKey`, `passphrase?`, `user?` (git) | git sources                                  |
| `s3`     | `accessKey`, `secretKey`, `region`         | s3 sources (AWS SigV4 signing)               |

Secrets are read on first use: a build only requires the secrets of the sources it uses. They
are only sent over `https` (except `localhost`), never embedded in URLs, and never forwarded on a
redirect to another host.

For CI, a second project file (for example `servercrafter-ci.yaml` with
`{ env: CI_JOB_TOKEN }`) can be passed with `-p` without touching the templates.

### Sources

| type     | fields                                    | serves                                           |
|----------|-------------------------------------------|--------------------------------------------------|
| `local`  | `directory`                               | `path` (file or directory)                       |
| `http`   | `baseUrl`, `credentials?`                 | `path`, and `download: maven` (Maven repository) |
| `s3`     | `baseUrl`, `credentials?` (type `s3`)     | `path`, and `download: maven`                    |
| `nexus`  | `baseUrl`, `repository?`, `credentials?`  | `path` (within `repository`), `download: maven` via the search API |
| `gitlab` | `baseUrl`, `project?`/`group?`, `credentials?` | `download: maven` and `download: gitlab-generic` |
| `git`    | `repository`, `ref?`, `credentials?`      | `path` (file or directory), cloned once per build |

## Templates

A template with an `id` is buildable. A template without an `id` is a **partial**, which only
serves as a parent; by convention its name starts with `_`.

```yaml
# yaml-language-server: $schema=../schemas/template.schema.json
version: 1
id: lobby
parents: [paper-common]

server:
  download: { type: papermc, project: paper, version: "1.21.4" }

plugins:
  - jarId: luckperms                 # unique identifier, merge key with parents
    name: LuckPerms.jar              # installed file name
    download: { type: luckperms, platform: bukkit }
  - jarId: core
    name: core.jar
    destination: plugins/Core/modules   # default: plugins
    requiredEnv: WITH_CORE              # installed only if WITH_CORE=true or 1
    download: { type: maven, source: nexus, groupId: com.example, artifactId: core, version: latest-snapshot, classifier: all }
  - { jarId: fawe, name: FAWE.jar, url: https://cdn.modrinth.com/…/FastAsyncWorldEdit.jar, sha256: d72941fb… }
  - { jarId: polar, name: polar.jar, source: static, path: anticheat/PolarLoader.jar }

worlds:
  - { name: world, source: maps, path: lobby.zip, extract: zip, stripComponents: 1 }

files: [files/common, files/lobby]

patches:
  - file: server.properties
    fields:
      - { field: motd, value: "${MOTD:-Welcome}" }

runtimePatches:
  - file: config/paper-global.yml
    fields:
      - { field: proxies.velocity.secret, value: "${CFG_VELOCITY_SECRET}" }
```

Unknown keys are errors (with line and column), and the JSON schemas in
[`schemas/`](schemas) provide IDE autocompletion via the `yaml-language-server` comment or the
`$schema` key.

## Inheritance

`parents` lists template names. The name `x` refers to `_x.yaml`, otherwise `x.yaml`: a
`lobby.yaml` template can therefore inherit from its content `_lobby.yaml` with `parents: [lobby]`.

Parents are applied in list order, depth-first, each ancestor only once (a parent shared by two
branches is applied only once), then the template itself:

| field                                 | merge                                                        |
|---------------------------------------|--------------------------------------------------------------|
| `server`                              | the last one declared wins                                   |
| `plugins` / `worlds`                  | by `jarId` / `name`: an entry replaces a parent's entry at the same position, `remove: true` removes it |
| `files`, `patches`, `runtimePatches`  | concatenated                                                 |

Cycles are detected. `servercrafter render <id>` prints the merged result and
`servercrafter list` the chain of each template.

An environment variant is a template that overrides some of its parent's plugins:

```yaml
# lobby-dev.yaml: the lobby with the locally built plugin
version: 1
id: lobby-dev
parents: [lobby]
plugins:
  - { jarId: core, name: core.jar, source: dev, path: core.jar }   # replaces the one from Nexus
  - { jarId: viaversion, remove: true }
```

## Artifacts and downloads

Each artifact (server, plugin, world) uses **exactly one** form:

- `url:` a direct http(s) URL;
- `source:` + `path:` a file (or directory) from a project source;
- `download:` a typed resolver, which finds the version and its checksums.

A download's `version` may reference environment variables, like patch values:
`version: "${BRANCH:-main}"` installs the `main` version unless `BRANCH` is set. A `--set`
override replaces the version before it is interpolated.

`sha256:` is accepted everywhere, in addition to the checksums published by the source. Every
download is written to a temporary file, verified, then renamed: an error response or a wrong
checksum never leaves a corrupted jar behind.

| type             | fields                                                                 | versions                                                         | checksums |
|------------------|------------------------------------------------------------------------|------------------------------------------------------------------|-----------|
| `papermc`        | `project` (paper, folia, velocity, waterfall), `version?`, `build?`, `channel?` (stable, beta, alpha) | `1.21.4`, `latest` (default: latest non-pre/rc version with a build in the channel) | sha256 |
| `modrinth`       | `project` (id or slug), `version?`, `loaders?`, `gameVersions?`, `asset?` | `latest` (latest release, default), `latest-published` (beta/alpha included), version number | sha512, sha1 |
| `github`         | `repository` (owner/name), `asset` (regex on the full name), `version?`, `credentials?` | `latest` (default, excluding pre-releases), `latest-published` (pre-releases included), tag | sha256 (GitHub digest) |
| `luckperms`      | `platform` (bukkit, velocity, fabric…)                                 | `latest` only                                                    | — |
| `maven`          | `groupId`, `artifactId`, `version`, `classifier?`, `extension?` + `source` (nexus, gitlab, http, s3) **or** `repository` (URL) + `credentials?` | exact version, `X-SNAPSHOT`, `latest`, `release`/`latest-release`, `latest-snapshot` | published `.sha256`/`.sha1`, or Nexus's |
| `gitlab-generic` | `source` (gitlab), `package`, `version`, `file`, `project?`            | exact                                                            | — |

For Maven, snapshots are resolved via `maven-metadata.xml` to the latest timestamped file
(`core-1.3.0-20260730.120000-2-all.jar`). With a `nexus` source, the search API picks the
version server-side and `repository` can override the source's repository. With a `gitlab`
source, `project`/`group` override the source's.

### Worlds

A world is a directory installed at `<destination>/<name>` from an archive (`extract: zip`,
`stripComponents` to strip the archive's root directory) or from a directory of a
`local`/`git` source. **An existing world is never replaced** (it contains the server's data),
unless `overwrite: true` is set.

### Pinning a version

```sh
servercrafter build lobby --set server=1.21.8 --set core=1.4.0-SNAPSHOT --set world:world=…
```

The key is `server`, a `jarId` or `world:<name>`. An unknown key, or an artifact pinned by `url`
or `path`, is an error.

## Overlays and patches

`files` lists directories, relative to the project, copied in order on top of the server after
the artifacts are installed (configs, datapacks, `eula.txt`…).

Patches then modify fields in `.properties`, `.yml`/`.yaml` and `.json` files:

- `field` is a dotted path (`proxies.velocity.secret`); a numeric segment indexes a list
  (`servers.0.name`). Missing keys are created.
- `value` is a string, a number or a boolean. Strings accept `${VAR}`,
  `${VAR:-default}` and `$$` for a literal `$`. A missing variable without a default is an error.
- `type` (`string`, `int`, `float`, `bool`) converts the value after interpolation:
  `{ field: max-players, value: "${MAX_PLAYERS}", type: int }`.
- Comments, key order and key case are preserved (YAML and `.properties` are edited in
  place, JSON is rewritten with the same order and indentation).

`patches` are applied at build time, with the build environment. `runtimePatches` are applied on
every server start by `servercrafter bootstrap`, with the container environment: this is where
passwords go, so they never end up in the image. The shipped files then contain a placeholder
value (`password: replace-at-runtime`).

## Build, manifest and diff

The build order is fixed:

1. loading, parent merging, validation;
2. resolving and downloading artifacts in parallel (`-j`, 4 by default), cancelling all
   downloads on the first failure;
3. removing jars that a previous build installed and that the template no longer declares;
4. copying the `files` overlays;
5. `patches`;
6. writing `servercrafter-manifest.json` (path, origin, version and sha256 of each artifact,
   plus the `runtimePatches`).

A rebuild in the same directory only re-downloads what changed: a file whose published
checksums match, or whose version and origin haven't changed since the previous build, is kept
(`--refresh` forces everything). `--clean` empties the directory first, but only if it contains
a manifest.

`servercrafter diff lobby -o server` resolves the template without downloading anything and
compares it with the server: `add`, `update` (with `old -> new` version), `remove`, `modified`
(file changed on disk since the build), `unknown` (source without version or checksum).
`--exit-code` exits with 2 if the server is not up to date.

## Bootstrap

`servercrafter bootstrap` is designed to be a container's entrypoint:

1. it verifies the sha256 of every file in the manifest (`--no-verify` to skip);
2. it applies the `runtimePatches` with the container environment;
3. it replaces its process with the command after `--` (`exec`), which therefore receives the
   container's signals directly.

## Commands

| command                                        | purpose                                                      |
|------------------------------------------------|--------------------------------------------------------------|
| `build <id\|file> -o <dir>`                    | builds a server (`--set`, `-j`, `--clean`, `--refresh`)      |
| `validate [id…] [--resolve]`                   | validates the project and templates (partials included); `--resolve` queries the sources |
| `list`                                         | buildable templates and their inheritance chain              |
| `render <id>`                                  | merged template                                              |
| `diff <id> -o <dir>`                           | what a build would change (`--all`, `--exit-code`)           |
| `bootstrap -t <dir> [-m manifest] -- cmd…`     | verification, runtime patches, then exec                     |

Global options: `-p/--project`, `--log-level` (`SERVERCRAFTER_LOG_LEVEL`), `--log-format text|json`,
`--sentry-dsn` (`SENTRY_DSN`). A template can also be referenced by its file path.

## Development

```sh
go test -race ./...
go build -o dist/servercrafter ./cmd/servercrafter
```
