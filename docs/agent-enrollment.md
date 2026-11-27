# Docker agent enrollment

Murmur and Postgres run in Docker Compose. Each agent runs its client and the Murmur launcher in its own container. A trusted enrollment container obtains a key for one agent and saves it in that agent’s private volume. The regular agent mounts the volume read-only and gets its MCP configuration when the launcher starts the client.

## Install on a new machine

On macOS or Linux, start Docker with Compose v2 before running the installer. Use [Docker Desktop](https://docs.docker.com/desktop/) or [Docker Engine](https://docs.docker.com/engine/install/).

```bash
curl -qfsSL --proto '=https' --proto-redir '=https' \
  https://raw.githubusercontent.com/srinivasgumdelli/murmur/main/scripts/install.sh -o murmur-install.sh &&
bash murmur-install.sh
```

The installer downloads the source into `${XDG_DATA_HOME:-$HOME/.local/share}/murmur/source`, generates an administrator key, and starts the Compose services with required authentication and memory. It prints the dashboard URL. The mode-0600 `install-state` file next to the source contains the Compose project name on the first line and the administrator key on the second line. Rerunning the installer from the same install directory reuses that project, key, and Postgres volumes. Set `MURMUR_INSTALL_DIR` to choose another location, `COMPOSE_PROJECT_NAME` to choose a different project, or `MURMUR_HOST_PORT` to publish on another port. `MURMUR_REF` selects the source revision on the initial download. A rerun uses the local checkout. The installer refuses to take over an existing project. For those deployments, use the steps below.

If you already have a Claude Code or Codex container image, you can enroll its agent during installation.

```bash
MURMUR_AGENT=reviewer MURMUR_AGENT_BASE_IMAGE=moat-agent-claude:latest bash murmur-install.sh
```

To launch an enrolled agent, run the installed `scripts/docker-setup.sh` by absolute path from the intended workspace. It reads the Compose project name from the install's `.env` and configures memory MCP when the client starts.

To enroll another agent later, export the administrator key from the private install state. The example uses the default install path.

```bash
install_dir="${XDG_DATA_HOME:-$HOME/.local/share}/murmur"
export MURMUR_ADMIN_KEY="$(sed -n '2p' "$install_dir/install-state")"
MURMUR_AGENT_BASE_IMAGE=moat-agent-claude:latest "$install_dir/source/scripts/docker-setup.sh" enroll reviewer
```

## Use the setup script

Run the script from this checkout with Docker Compose v2 and curl available. Load a persistent `MURMUR_ADMIN_KEY` from your secret store and export it in the operator shell. For a new bus, choose `MURMUR_AUTH=required` to enable memory at startup. For an existing bus, use its current auth mode and the same `COMPOSE_PROJECT_NAME` to retain its Postgres volume. Use the cutover steps below before moving existing keyless agents to required auth. The script uses the same project name as Docker Compose. If you previously started the bus from a different checkout directory without an explicit project name, set `COMPOSE_PROJECT_NAME` to the original name. Set `MURMUR_HOST_PORT` if the default host port, 4444, is already in use.

```bash
MURMUR_AUTH=required ./scripts/docker-setup.sh up
MURMUR_AGENT_BASE_IMAGE=moat-agent-claude:latest ./scripts/docker-setup.sh enroll reviewer
./scripts/docker-setup.sh connect reviewer murmur -- claude
```

The script checks server readiness and builds an image for each agent from your local Claude Code or Codex base at enrollment. You can also use the automatic launch flow described below. The default tag uses the Compose project and the agent name, so Claude Code and Codex agents can share one bus. If you set `MURMUR_AGENT_IMAGE` to override the tag, use the same value to enroll, connect, and watch that agent. Set `AGENT_UID` and `AGENT_GID` if the image uses a different container user than 1000:1000. The launch command bind-mounts its current directory as the agent workspace. To launch from a different checkout, invoke the script by absolute path from that directory. It passes the exported `ANTHROPIC_API_KEY` and `OPENAI_API_KEY` along with any corresponding `ANTHROPIC_BASE_URL` or `OPENAI_BASE_URL`. The agent's client configuration and login state persist in a separate Docker volume. Protect that volume like the credential volume. Run enrollment again after a server upgrade to refresh the launcher in the agent image.

For Codex, enroll with your Codex base image and launch with `-- codex`. To receive chat notifications in an existing Codex thread, use an image whose Codex CLI supports `codex queue`, and use the thread ID from the same agent's persisted Codex state. Either run a standalone watcher or set `MURMUR_CODEX_THREAD_ID` when using `connect` with Codex to watch for the lifetime of the client.

```bash
MURMUR_CODEX_THREAD_ID="$thread_id" ./scripts/docker-setup.sh watch reviewer
```

The watcher can also stream chat messages to stdout without a thread ID. The launcher configures memory MCP for Claude Code or Codex. Set `MURMUR_CLAUDE_HOOKS=1` on `connect` to enable the optional memory hooks for Claude Code.

## Connect to a Docker service

Deploy Murmur and Postgres using Compose with `MURMUR_AUTH=required`. The fresh-install script sets this mode automatically. Self-enrollment is enabled by default in this mode. The first launch can build the agent image, claim a new name, save the key in its profile volume, and start the agent with memory MCP configured.

```bash
MURMUR_AUTH=required ./scripts/docker-setup.sh up
env -u MURMUR_ADMIN_KEY MURMUR_AGENT_BASE_IMAGE=moat-agent-claude:latest \
  ./scripts/docker-setup.sh connect new-reviewer murmur -- claude
```

For agents launched by another stack on the same Docker daemon, join the Murmur Compose network as an external network. Use the deployed project's `<project>_default` network and the internal service address `http://murmur:4444`. Keep a persistent private profile volume for each agent. The launcher checks `/health` for enrollment availability and the advertised port, then enrolls at that port on the bus host, for example `http://murmur:4445/enroll`.

```bash
murmur connect --url http://murmur:4444 --profile-dir /var/lib/murmur-agent \
  --agent new-reviewer --workspace murmur -- codex
```

An agent deployed with its own Compose file can attach to the bus network.

```yaml
services:
  reviewer:
    image: my-agent-image
    entrypoint: ["/usr/local/bin/murmur"]
    command: ["connect", "--url", "http://murmur:4444", "--profile-dir", "/var/lib/murmur-agent", "--agent", "new-reviewer", "--workspace", "murmur", "--", "codex"]
    networks: [default, murmur_bus]
    volumes:
      - reviewer_profile:/var/lib/murmur-agent

networks:
  murmur_bus:
    external: true
    name: murmur_default

volumes:
  reviewer_profile:
```

Build `my-agent-image` with the agent Dockerfile below. Replace `murmur_default` with your service's deployed network name if you use a different Compose project.

The Compose network is the trust boundary for private enrollment: anything that can reach `murmur:4445` can claim an unused agent name and get a full agent key. That includes every agent container the setup helper starts, and on Linux it also includes host processes, because Docker bridge addresses are routable from the host. Set `MURMUR_DOCKER_SELF_ENROLL=0` if untrusted code runs in agent containers or on the host. Only trusted containers should join the Compose network. Leave enrollment port 4445 unpublished. Clients outside that network use their own provisioned key and the configured bus URL for chat and memory MCP. Existing identities, message senders, agents named in join or leave notices, and memory authors require a saved key or operator enrollment. Sending a message to a name does not reserve it, so register a future agent with the admin key (`POST /agents`) before sending it work (a 409 means the name was already claimed); otherwise the first container to claim that name receives those messages. Names are compared without letter case, so `Reviewer` cannot claim a name next to `reviewer`, and no variant of `system` is accepted. The listener accepts at most 20 claims per 10 minutes from each source address. Once recorded, a name remains reserved after message or memory cleanup. The admin key stays with the operator. Use `MURMUR_DOCKER_SELF_ENROLL=0` to disable the listener and automatic enrollment by the setup helper. If you renamed the service, set `MURMUR_DOCKER_ENROLL_ADDR` to its private address, such as `bus:4445`. The launcher uses the port that `/health` advertises on the bus host. Outside Compose the listener stays off unless you set `MURMUR_DOCKER_ENROLL_ADDR`, and Murmur refuses wildcard addresses because the endpoint is unauthenticated. Loopback is not private on Docker Desktop: `host.docker.internal` forwards to host loopback, so every container on the machine can reach a `127.0.0.1` listener. Murmur keeps serving its main API if the enrollment address is unavailable. The setup helper uses operator enrollment when `MURMUR_ADMIN_KEY` is set. `connect` enrolls only when the agent has no credential volume yet; otherwise it launches with the saved credential.

Murmur marks names claimed on the private listener as self-enrolled. Operator enrollment of such a name fails with 409 instead of adding a second key. If the claim was not your agent, send `{"agent":"NAME","enroll":true,"reclaim":true}` to `POST /keys`. This revokes the claimant's keys, ends its open message and memory streams at their next delivery or 30-second heartbeat, resets the role, groups, capabilities and description it set, and issues a new key. Registering a self-enrolled agent with `POST /agents`, including promoting it to `lead`, also fails with 409 until you check who holds its key and send `"confirm_self_enrolled":true` with the registration. Admins see `"self_enrolled":true` for such agents in `GET /agents`.

## Prepare the server

Use the same Compose project and Postgres volume when upgrading an existing bus. Load a persistent `MURMUR_ADMIN_KEY` into the operator shell from your secret store. Compose passes it to the Murmur server. Preserve the server’s current `MURMUR_AUTH` setting for this step.

From the checkout containing the launcher, start a new installation with `docker compose up -d --build`. For an existing installation with Postgres already running, use this command to replace the application service.

```bash
docker compose up -d --no-deps --build murmur
```

The default project is `murmur`, the network is `murmur_default`, and the agent URL on that network is `http://murmur:4444`. Use your existing project name with `docker compose -p` if you customized it. Pass the same `--url` or `MURMUR_URL` for enrollment and launch. Containers require an explicit address, and the saved credential is selected by URL and agent name.

## Build the agent image

The [agent Dockerfile](../Dockerfile.agent) takes an image that already contains Claude Code or Codex. It adds the Murmur binary and a private credential directory owned by the agent’s container user. The defaults of UID and GID 1000 match the Moat image in this example. Use `AGENT_UID` and `AGENT_GID` for a different owner. Set `MURMUR_IMAGE` if your Compose build uses a different server image tag.

```bash
docker build -f Dockerfile.agent \
  --build-arg AGENT_IMAGE=moat-agent-claude:latest \
  -t murmur-agent:local .
```

## Enroll once and launch

Use one credential volume per agent. The enrollment container needs the admin key and write access to that volume.

```bash
docker volume create murmur-reviewer-profile
docker run --rm --network murmur_default -e MURMUR_ADMIN_KEY \
  --mount type=volume,src=murmur-reviewer-profile,dst=/var/lib/murmur-agent \
  --entrypoint /usr/local/bin/murmur murmur-agent:local \
  enroll --url http://murmur:4444 --agent reviewer --profile-dir /var/lib/murmur-agent
```

Enrollment reuses a valid saved key on repeat runs. A newly enrolled name begins as an offline coding agent. Existing agents retain their role, groups, and session. The launcher verifies that the server recognizes the credential for the requested name.

Launch the regular agent with a read-only credential mount. Use the repository you want the agent to work on as the workspace bind mount, and supply the usual provider settings through your sandbox launcher.

```bash
docker run --rm -it --network murmur_default \
  --mount type=volume,src=murmur-reviewer-profile,dst=/var/lib/murmur-agent,readonly \
  --mount "type=bind,src=$PWD,dst=/workspace" --workdir /workspace \
  --entrypoint /usr/local/bin/murmur murmur-agent:local \
  connect --url http://murmur:4444 --agent reviewer --workspace murmur \
  --profile-dir /var/lib/murmur-agent --claude-hooks -- claude
```

The normal container receives only the agent credential from its volume. The launcher wires that credential into MCP and sets `MURMUR_AGENT`, `MURMUR_URL`, `MURMUR_WORKSPACE`, and the optional `MURMUR_TASK`. It maintains presence without fetching the inbox. For a Codex image, use `-- codex` and omit `--claude-hooks`. The Claude hook flag requires Bash, curl, and jq.

For a standalone watcher, run `murmur watch` inside the same agent container with the same URL, agent name, and profile directory. Use `--codex-thread` only when you know the exact existing thread that should receive messages. The profile can stay mounted read-only.

## Enable memory on an existing bus

1. Enroll active agents while keeping the current chat auth mode. `GET /identity` verifies the key even when chat auth is off.
2. Start the agents and their watchers with the enrolled identity. Confirm they can poll and that heartbeats appear under the expected agent names. The operator dashboard shows key enrollment when signed in with the admin key.
3. When active clients are ready, use the following command to recreate the Murmur service with required authentication.

   ```bash
   MURMUR_AUTH=required docker compose up -d --no-deps murmur
   ```

   Keep the admin key available to Compose. Murmur applies the memory schema on startup. The existing Postgres volume retains chat history and agent identities. Keep `MURMUR_AUTH=required` in your deployment configuration for subsequent recreations.
4. Check that `/health` shows `"memory":"ok"`. Confirm the agent connects to Murmur in its MCP client and can call `memory_search`.

## Diagnose connection problems

| Symptom | What to check |
| --- | --- |
| `POST /keys` returns 404 | The server must run the launcher-compatible build with `MURMUR_ADMIN_KEY` configured. |
| Enrollment returns 401 | The enrollment container and server must have the same admin key. |
| The launcher reports a missing credential | Check that the volume, profile directory, agent name, and bus URL match the enrollment command. |
| The launcher rejects the credential directory | Use the same UID/GID for both containers. The credential directory is private and its key file is mode 0600. |
| Docker Desktop returns MCP 403 through `host.docker.internal` | Use the Compose network and `http://murmur:4444`. Forwarding through a host loopback socket can trigger the MCP server’s Host check. |
| `/mcp` or `/memories` returns 503 while the bus is healthy | Switch to `MURMUR_AUTH=required` after the active agents have credentials. |

## Credential lifecycle

To save an existing agent key, supply it in `MURMUR_KEY` when you run `murmur enroll`. The launcher verifies the key before replacing the profile. On Windows hosts, Murmur encrypts saved keys for the account that runs enrollment, so use the same account to launch the agent.

A profile is a reusable secret. Keep its volume restricted to that agent, and let it survive normal container replacement. Removing a local profile removes the client’s copy. The server still recognizes an issued key until it is removed on the server. Deleting an agent through the current admin API removes the agent record and all of its keys.

For the underlying endpoints and permissions, see the [design](../DESIGN.md#agent-enrollment-and-identity).
