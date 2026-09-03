# Upgrading OpenConvo

OpenConvo never controls the host Docker daemon. The dashboard checks the
latest stable GitHub release and, when the release is compatible, gives the
administrator a host command to copy. Running that command remains an explicit
host operation.

The release check sends a normal unauthenticated request to GitHub's Releases
API. It sends no archive data, credentials, configuration, or running version,
and successful results are cached for six hours. A failed check is cached for
15 minutes so an offline installation does not repeatedly contact GitHub.

## Compatible Compose upgrades

The dashboard offers the command for these upgrades:

- before 1.0, releases in the same minor line (`0.1.0` to `0.1.1`);
- from 1.0 onward, releases in the same major line (`1.2.0` to `1.3.0`).

Other upgrades may change deployment requirements and must be reviewed
manually. The dashboard still reports the release, but does not present it as
a routine command upgrade.

From the directory containing `compose.yaml`, run the command shown by the
dashboard:

```bash
./scripts/upgrade.sh 0.1.1
```

This pulls that exact published application image, creates a PostgreSQL dump
and a portable archive under `./backup/`, records the new image in `.env`, and
replaces only the OpenConvo container. A pull or backup failure leaves the
installed version unchanged. PostgreSQL and attachment data remain in their
named volumes.
Ensure the host has enough free space for the portable archive; pass a separate
off-machine destination to `scripts/backup.sh` when appropriate.

`compose.yaml` has no build section, so a stray `--build` on this command
cannot substitute the local checkout for the image that was just pulled.
Building is opt-in through `compose.dev.yaml`, below.

After the container starts, verify it:

```bash
docker compose ps
docker compose exec openconvo openconvo version
docker compose exec openconvo openconvo healthcheck
docker compose logs --tail=100 openconvo
```

Schema migrations run automatically on startup when
`OPENCONVO_AUTO_MIGRATE=true`, the default. Migrations are additive and
up-only.

## Manual compatibility-boundary upgrades

Before crossing a compatibility boundary:

1. Read the target release notes for configuration, Compose, PostgreSQL, and
   migration requirements.
2. Put the output of `scripts/backup.sh` somewhere outside the OpenConvo host.
3. Update the checked-out `compose.yaml` and any explicitly documented `.env`
   values.
4. Pull and recreate the application container.
5. Verify health, version, logs, archive counts, and a sample attachment.

Never rerun `scripts/install.sh` over an existing installation. It is an
initial installer and deliberately refuses to replace `.env`.

## Development checkouts

An installation built directly from a branch or commit is a development build;
the dashboard cannot meaningfully order it against stable releases. Update it
from its checkout:

```bash
./scripts/backup.sh
git pull --ff-only
docker compose -f compose.yaml -f compose.dev.yaml up -d --build --force-recreate openconvo
```

The overlay is what supplies the build section; without it Compose has only
the published image to run.

The resulting binary may still identify itself as a development build. Use the
published image for stable release tracking.

## Rollback and recovery

Container recreation does not delete named volumes. Never use
`docker compose down -v` during an upgrade or rollback because `-v` deletes
those volumes.

If a new container fails before applying migrations, recreate OpenConvo with
the previous versioned image. The normal image is recorded in `.env`; for a
temporary rollback, pin the older tag in a `compose.override.yaml` next to it.
Compose reads that file automatically, and it is untracked, so updating the
release files leaves it alone:

```yaml
services:
  openconvo:
    image: ghcr.io/openconvo/openconvo:0.1.0
```

```bash
docker compose pull openconvo
docker compose up -d --force-recreate openconvo
```

Pull before recreating so a missing tag or registry error stops before the
running container is replaced. The default `compose.yaml` has no build section
and therefore cannot fall back to building the working tree. Delete the
override file to return to the exact image recorded in `.env`.

Once an up-only migration has applied, assume an older application may not
understand the database. Stop OpenConvo and restore the pre-upgrade database
dump before using the older image. See
[restoring a database backup](self-hosting.md#restoring-a-database-backup).

PostgreSQL major-version upgrades are separate from application upgrades and
must follow PostgreSQL's dump/restore or `pg_upgrade` procedures described by
the relevant release notes.

## Maintainer release requirement

Publishing a GitHub release builds attested `linux/amd64` and `linux/arm64`
images and pushes the semantic-version tags to GitHub Container Registry. The
first time the package is created, a repository administrator must confirm
that `ghcr.io/openconvo/openconvo` is public so self-hosters can pull it
without registry credentials. Organization package defaults can otherwise
leave a new container package private.
