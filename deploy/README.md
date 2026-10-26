# Linux deployment

For containers, use the [Docker and Compose instructions](DOCKER.md), including the persistent-disk setup for a cloud VM.

Use an account dedicated to the server. These commands describe a deployment; they are not executed by the build scripts.

1. Create the service account, install the Linux binary for the server's architecture, and place the initial owner's public key outside home directories:

   ```sh
   sudo useradd --system --home-dir /var/lib/crease --shell /usr/sbin/nologin crease
   sudo install -m 0755 crease-linux-amd64 /usr/local/bin/crease
   sudo install -d -m 0755 /etc/crease
   sudo install -m 0644 owner.pub /etc/crease/owner.pub
   ```

2. Install the [systemd service](crease.service):

   ```sh
   sudo install -m 0644 deploy/crease.service /etc/systemd/system/crease.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now crease
   sudo journalctl -u crease
   ```

   systemd creates `/var/lib/crease` with the service user's ownership. Initialization reads `/etc/crease/owner.pub` once. Later starts preserve the original owner. Keep `/var/lib/crease` on a persistent local volume.

3. Allow inbound TCP port 2222 through the host firewall and cloud security group for the intended clients. The application SSH service is separate from the host's administrative SSH service.

4. Copy the host-key fingerprint printed in the service log through a trusted channel. Compare it with the client's first connection prompt before accepting it.

5. For a consistent backup, stop the service and run the binary as the service user, using a private backup directory that user can write:

   ```sh
   sudo systemctl stop crease
   sudo -u crease /usr/local/bin/crease backup \
     --data /var/lib/crease --output /path/to/private-backups/snapshot
   sudo systemctl start crease
   ```

   The destination must not exist and must be outside the data directory. Backups contain the server host private key and any committed secrets. Encrypt and restrict backup storage as appropriate for the repository.

To restore, create a new data directory with `restore-server`, preserve service-user ownership, and point the service at that directory. Test restoration on an isolated host before replacing a live installation. `restore-server` verifies the backup manifest and requires an absent destination.
