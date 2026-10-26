# Docker server

The image runs the Crease SSH server as UID/GID `65532`, using a read-only root filesystem. Compose stores the database, repository objects, and SSH host key in a persistent volume. It mounts only the owner's **public** key; the owner's private key stays on the client. Build inputs are restricted by `.dockerignore`.

## Start locally

Install Docker with Compose v2. From the project root, generate a dedicated local owner identity once:

```sh
mkdir -p deploy/local
chmod 700 deploy/local
ssh-keygen -t ed25519 -f deploy/local/owner -C 'Crease local Docker owner'
chmod 644 deploy/local/owner.pub
docker compose up -d --build
docker compose logs crease
```

The generated files are ignored by Git. For the initial automated local setup, the generated key has no passphrase. If you set one, load the key with `ssh-add deploy/local/owner` and omit `--key` from client commands so Crease uses the SSH agent. To use an existing identity instead, set `CREASE_OWNER_KEY` to its public-key path before running Compose. Initialization registers that key once; changing the mounted file does not change an existing server's owner.

The default endpoint is `ssh://localhost:2222`, reachable only from this computer. Verify the host fingerprint printed in the logs when the CLI first connects:

```sh
crease --key "$PWD/deploy/local/owner" create ssh://localhost:2222/my-project ./my-project
```

To use the generated owner identity without repeating its path, run this once from the Crease source directory:

```sh
crease key use "$PWD/deploy/local/owner"
```

Teammates can share their own public key with `crease key share`. From inside the repository, grant it with `crease access grant --role write` and paste the key, or use `crease access grant --clipboard --role write`. The [access instructions](../README.md#access-and-review) cover identity creation and supported roles.

Routine operations:

```sh
docker compose logs -f crease
docker compose stop
docker compose start
docker compose up -d --build
docker compose down
```

`down` removes the container and network while preserving the data volume. `down -v` **deletes the server's data**. Rebuilds and container replacement preserve the host identity and repositories. Run one server per data directory.

## Linux VM with a persistent disk

Use Docker Engine and Compose v2 on a Linux VM. Put server data on an attached, mounted persistent disk. For example, after mounting the disk at `/mnt/crease`, prepare the data directory and bootstrap public key:

```sh
sudo install -d -o 65532 -g 65532 -m 0700 /mnt/crease/data
sudo install -d -m 0755 /etc/crease
sudo install -m 0644 /path/to/your/owner.pub /etc/crease/owner.pub
```

Set the paths and listener, then apply the disk override from the project root:

```sh
export CREASE_DATA_DIR=/mnt/crease/data
export CREASE_OWNER_KEY=/etc/crease/owner.pub
export CREASE_BIND=0.0.0.0
docker compose -f compose.yaml -f deploy/compose.disk.yaml up -d --build
docker compose -f compose.yaml -f deploy/compose.disk.yaml logs crease
```

Use those same exports and both `-f` arguments for later Compose commands. Ensure the disk is mounted before starting the container. Restrict inbound TCP `2222` to the intended clients in the cloud and host firewalls. Clients connect to `ssh://VM_ADDRESS:2222/repository`; the VM's administrative SSH port is separate. No HTTP proxy or TLS termination is required: the application uses SSH encryption and authentication.

The override uses the host disk directory in place of the default named volume. The directory must already exist and be writable by UID `65532`. The server restarts with Docker unless explicitly stopped. Configure the VM's boot sequence to mount the disk before starting Docker/Compose.

## Back up and restore

Stop the server before running `backup`. The following example uses the default local Compose setup and writes to an ignored local directory:

```sh
mkdir -p deploy/local/backups
# On Linux, make this directory writable by the container user.
sudo chown 65532:65532 deploy/local/backups
docker compose stop crease
docker compose run --rm --no-deps \
  -v "$PWD/deploy/local/backups:/backups" \
  crease backup --data /var/lib/crease --output /backups/snapshot
docker compose start crease
```

Choose a new output name for every backup. A backup contains committed secrets and the server's host private key; protect its storage. For cloud deployments, apply the disk override and environment above to the backup command too. Copy completed backups off the VM.

Restore into an absent directory on the persistent disk, while the server is stopped:

```sh
sudo install -d -o 65532 -g 65532 -m 0700 /mnt/crease/restore
docker run --rm --user 65532:65532 \
  -v /path/to/backups:/backups:ro \
  -v /mnt/crease/restore:/restore \
  crease:local restore-server --input /backups/snapshot --data /restore/restored-data
```

After verifying the restored server, point `CREASE_DATA_DIR` at `/mnt/crease/restore/restored-data`. Keep the previous data directory until the restoration has been checked.

## Build and verify

```sh
docker compose build
docker build --target test -t crease:test .
make build
python3 scripts/docker-smoke.py
```

The test stage runs the Go suite with the race detector inside Linux. It is separate from the runtime image. The smoke test uses disposable containers, volumes, and keys to check SSH access, push/clone, persistence after container replacement, and backup/restore. It cleans up its own resources and leaves the regular server alone. To build for an x86 Linux VM on an ARM computer:

```sh
docker buildx build --platform linux/amd64 -t crease:local --load .
```

To build for both CPU architectures for a registry, use `--platform linux/amd64,linux/arm64` with a registry tag and `--push`. Publishing and cloud deployment are separate from this local setup.
