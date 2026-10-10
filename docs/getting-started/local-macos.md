# Run Astrion locally on macOS

This guide runs the complete vertical slice on one Mac:

- PostgreSQL;
- a Temporal dev server;
- the API;
- the worker;
- the web console;
- the **mock agent** on the **local provider**.

Jobs run as local processes, and no Docker or Kubernetes is needed.

> The local provider and filesystem artifact store are for development only.
> Production mode (`ASTRION_ENV=production`) refuses both.

Time: about 15 minutes. Works on Apple Silicon and Intel.

## 1. Install prerequisites

With [Homebrew](https://brew.sh):

```bash
brew install go node postgresql@17 temporal git
```

| Tool | Minimum | Check |
|---|---|---|
| Go | 1.26 | `go version` |
| Node.js | 22.12 (or 20.19) | `node --version` |
| PostgreSQL | 15+ (17 recommended) | `postgres --version` |
| Temporal CLI | 1.x | `temporal --version` |
| Git | 2.30+ | `git --version` |

`postgresql@17` is keg-only, so add its binaries to your `PATH` (zsh):

```bash
echo 'export PATH="$(brew --prefix postgresql@17)/bin:$PATH"' >> ~/.zshrc
source ~/.zshrc
```

## 2. Get the code and build

```bash
git clone https://github.com/deepakppatil/Astrion.git
cd Astrion

go build -o bin/ ./cmd/...        # astrion-api, astrion-worker, astrion-supervisor, mock-agent

cd web && npm ci && npm run build && cd ..   # produces web/dist
```

## 3. Start PostgreSQL and create the database

```bash
brew services start postgresql@17
createdb astrion
```

Homebrew's PostgreSQL trusts local connections for your macOS user, so the
connection string is:

```bash
export ASTRION_DATABASE_URL="postgres://$(whoami)@localhost:5432/astrion?sslmode=disable"
```

The API and worker create the schema automatically on start.

## 4. Start Temporal (terminal 1)

```bash
temporal server start-dev --db-filename temporal.db
```

- gRPC listens on `localhost:7233`; Astrion's default is `127.0.0.1:7233`.
- The Temporal Web UI is at <http://localhost:8233>. Every Astrion job
  appears there as a workflow named `job-<jobId>`.
- `--db-filename` keeps workflow history across restarts. Omit it for a
  throwaway in-memory server.

## 5. Optional: a demo repository

Commands that read code need a repository. Create a local one:

```bash
mkdir -p ~/astrion-demo && cd ~/astrion-demo
git init -b main
echo "# Demo" > README.md
git add . && git -c user.name=demo -c user.email=demo@example.com commit -m init
cd -
```

Use `file:///Users/<you>/astrion-demo` as `ASTRION_DEMO_REPO_URL` below.
It is attached to the seeded demo workspace on the next API start.

## 6. Start the API (terminal 2)

Run from the repository root:

```bash
export ASTRION_DATABASE_URL="postgres://$(whoami)@localhost:5432/astrion?sslmode=disable"
export ASTRION_HTTP_ADDR=127.0.0.1:8080
export ASTRION_WEB_DIR=web/dist
export ASTRION_DEMO_REPO_URL="file://$HOME/astrion-demo"   # optional
./bin/astrion-api
```

On first start it migrates the database and seeds:

- the default tenant;
- the SDD framework and its commands;
- a mock agent profile;
- a demo workspace.

Check it: `curl http://127.0.0.1:8080/readyz` → `{"status":"ready"}`.

## 7. Start the worker (terminal 3)

The worker launches `astrion-supervisor` and `mock-agent` from your `PATH`.
Run from the repository root so both processes share `.astrion/` (the local
artifact store):

```bash
export ASTRION_DATABASE_URL="postgres://$(whoami)@localhost:5432/astrion?sslmode=disable"
export PATH="$PWD/bin:$PATH"
./bin/astrion-worker
```

In development the worker registers the Temporal namespace if needed. It
then starts polling the `astrion-jobs` task queue.

## 8. Use it

Open <http://127.0.0.1:8080> and pick a development persona:

- **Dana** (developer + QA);
- **Bea** (business analyst);
- **Arch** (architect);
- **Rel** (release engineer);
- **Admin**.

Try the human-in-the-loop flow:

1. Sign in as **Bea** and go to **Run command**. Choose `/write-spec`, enter a
   spec name, and submit.
2. The job page streams progress live. The mock agent asks *"/write-spec:
   which level of detail should the output target?"* and the job enters
   `WAITING_FOR_INPUT`. The
   environment is released while it waits.
3. Answer from the job page or the **Inbox**. The job resumes from its
   checkpoint, produces a `specification` artifact, cleans up, and ends
   `SUCCEEDED`.

Mock-agent tags change its behaviour when you include them in any input:

| Tag | Effect |
|---|---|
| `[mock:no-hitl]` | Completes without asking |
| `[mock:approval]` | Asks for an approval instead |
| `[mock:fail]` | Fails |
| `[mock:slow]` | Slow steps (try **Cancel**) |
| `[mock:secret-leak]` | Prints a fake token; check that the logs are redacted |

From the command line:

```bash
curl -s -X POST http://127.0.0.1:8080/api/v1/jobs \
  -H "Authorization: Bearer dev-analyst-token" -H "Content-Type: application/json" \
  -H "Idempotency-Key: demo-$(date +%s)" \
  -d '{"workspaceId":"<id from GET /api/v1/workspaces>","command":"/write-spec","inputs":{"specName":"Demo [mock:no-hitl]"}}'
```

The full API is described in [`api/openapi.yaml`](../../api/openapi.yaml).

## Option: hot-reloading console

To work on the UI, run the Vite dev server instead of serving `web/dist`. It
proxies `/api` to `127.0.0.1:8080`, or to `ASTRION_API_URL` if set:

```bash
cd web && npm run dev     # http://localhost:5173
```

## Stop and reset

- Stop each terminal with `Ctrl+C`. Temporal keeps every job's state, so
  in-flight jobs carry on when the worker is restarted.
- To start over, run the following and restart Temporal without
  `temporal.db`:

  ```bash
  dropdb astrion && createdb astrion
  rm -rf .astrion temporal.db
  ```

- `brew services stop postgresql@17` stops the database.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `createdb: command not found` | The keg-only `PATH` step in §1 was skipped. |
| API exits with `connect: connection refused` (5432) | Postgres isn't running: `brew services list`. |
| Worker logs `failed reaching server` | Temporal (terminal 1) isn't running. |
| Job stuck in `PROVISIONING`; worker log shows `executable file not found` | Run the worker with `bin/` on `PATH` (§7). |
| Artifact download returns 404 | API and worker were started from different directories; both must share `.astrion/`, or set the same absolute `ASTRION_DATA_DIR` for both. |
| macOS asks to allow incoming connections | Allow, or keep `ASTRION_HTTP_ADDR=127.0.0.1:8080` (loopback only). |
| Port 8080 in use | Use another `ASTRION_HTTP_ADDR` **and** set `ASTRION_EXEC_API_URL` to match on the worker. |
