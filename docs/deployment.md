# Deployment

Two supported shapes: the native single binary (best developer
experience) and a containerized controller. Both assume a running Herdr
server and a reachable Docker daemon on the host — Herder is a client of
both, never a replacement.

## Native (single binary)

```bash
go build -o herder ./cmd/herder
./herder init                 # ~/.config/herder/config.yaml
./herder config validate
./herder daemon               # status view on server.listen (127.0.0.1:8787)
```

Requirements on the host: `herdr` (with its server running), `docker`
(with the daemon up), the coding agents you configured, and `gh auth
login` for delivery.

### systemd unit

```ini
# /etc/systemd/system/herder.service
[Unit]
Description=Herder control plane
After=docker.service
Requires=docker.service

[Service]
Type=simple
User=herder
ExecStart=/usr/local/bin/herder --config /etc/herder/config.yaml daemon
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Run the service as the same user that owns the Herdr session and the
state directory — the daemon shells out to `herdr` and `docker` as that
user.

## Container (controller)

The repo ships a `Dockerfile` for the controller image and
`examples/docker-compose.yaml` for a runnable composition. Build and run:

```bash
docker build -t herder .
docker compose -f examples/docker-compose.yaml up
```

### Privilege boundary

The controller container is the ONLY component granted the Docker socket
and the Herdr socket: it needs Docker to provision sandboxes and Herdr
to place agents in host-visible panes. Worker containers are created by
the controller with `--cap-drop ALL`, `no-new-privileges`, resource caps,
and exactly one mount — the task workspace. Workers NEVER receive the
Docker socket, the Herdr socket, `GH_TOKEN`, the GitHub App key, or the
Herder database; mounting the controller's sockets or credentials into a
worker collapses the sandbox boundary and gives agent-controlled code
host-level control. The controller's socket access is itself the
privilege boundary: treat the controller container as host-equivalent.

### Host Herdr server required

The container does not run Herdr. It is a socket client of the host's
Herdr server, reached through a bind-mounted `HERDR_SOCKET_PATH`. Start
`herdr` on the host first; the compose file mounts the socket into the
controller.

### PATH PARITY

Bind-mount the Herder state directory at the **identical absolute path**
inside the container as on the host. Worker containers are created with
`docker --volume <workspace>:/workspace`, and that source path is
interpreted by the host Docker daemon — a workspace recorded as
`/home/u/.local/state/herder/sandboxes/<task>` must resolve to the same
host directory from inside the controller container. Different mount
points break provisioning.

### UID/GID notes

- `herdr.sock` is `0600`, owned by the UID running the host Herdr server:
  the controller must run as that UID (or the mount is unreadable).
- `docker.sock` is group `docker`: the controller needs that GID
  (`group_add` in compose) or root-equivalent access.
- Worker containers run as the workspace owner's UID — keep the state dir
  ownership consistent between host and container.

### Webhook ingress

GitHub must reach `POST /v1/webhooks/github` on `server.listen`
(default `:8787`). On a developer machine that means a tunnel
(`gh webhook forward`, ngrok, cloudflared, …) pointed at the controller;
in a private network, publish the port to wherever GitHub can route.
Deliveries are deduplicated by `delivery_id`, so retries and redeliveries
are safe.
