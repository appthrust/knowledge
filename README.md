# Knowledge

The first-party Knowledge built-in app UI, served by `cmd/server`.
It follows Directory's Vite / React / TanStack Query and Router package layout,
with Japanese and English copy from `@appthrust/platform-dashboard-i18n`.
The `appthrust-language` cookie preserves the language choice (Japanese by default).

## Screens

- `/`: search playground, collection filters, hybrid / lexical / vector mode,
  topK 1–50 (default 8), ranked document/chunk text and score. If `/config` says
  `llm.configured`, the LLM options `rerank` and `expand` (official terms and
  rephrasings, shown as chips from the response's `expanded`) are offered and
  Ask streams citations, tokens and completion from `/answer`.
- `/collections`: list, create and delete collections; comma/newline-separated
  visibility group IDs. Empty groups mean project-wide visibility.
- `/documents`: per-collection documents; synchronously ingest up to 1 MiB of
  Markdown, inspect chunk counts and delete documents.
- `/sources`: add a Notion source (integration token, root page IDs and database
  IDs) or an esa source (access token, team subdomain, optional esa search query,
  optional WIP inclusion). Sync now enqueues a background job (`202 {job}`); while
  a source's latest job (`sync`) is queued or running, `GET /sources` is polled
  every 2 s and the row shows documents/chunks so far (against the total when the
  source reports one); a failed job shows its error above the last sync. The last
  sync block shows the counts, completion time and individual errors of the last
  finished job. Tokens are write-only and never read back; when `/config`
  reports `notion.platformManaged` / `esa.platformManaged`, the token input is
  replaced by a link to the Project's built-in app settings and no token is sent.
  Notion and esa permissions are not copied: the collection visibility applies
  to imported documents.

The header reads project identity and the public URL from `/config`; the footer
reads `/releasez` (release version and source revision; full identity on hover).
It also shows the embedding source and dimensions from `/config`: `embedding`
is the built-in Embedding app (in-cluster), `openai` is OpenAI's hosted endpoint,
`custom` is another explicit endpoint, and `hashing` is local feature hashing.
Search summaries use the same localized source labels. The API's
`embedding.provider` remains the wire protocol; `embedding.source` is the
display identity, falling back to `provider` for older servers. When
`llm.configured` is true, the footer includes `llm.provider` (`gateway` is
labelled AI gateway); unconfigured LLMs omit the provider in `/config`.
While `/config` reports `embedding.backfill.state` `queued` or `running` (chunks
from a previous embedding model being re-embedded), every page shows a banner
with the remaining document count and `/config` is polled every 5 s.
Missing release identity does not prevent using the application.

## Local development

Use Go 1.26 or newer and Bun 1.4.1. From `web/`, install the published
GitHub Packages dependencies with `NODE_AUTH_TOKEN="$(gh auth token)" bun install --frozen-lockfile`.
Then start the mock and development server:

```sh
bun run mock
# Another terminal:
bun run dev
```

Open `http://127.0.0.1:5208`. The mock listens on `127.0.0.1:5206`; override with
`KNOWLEDGE_PORT` / `KNOWLEDGE_MOCK_PORT`. Vite proxies `/api`, `/config` and
`/releasez` and `/launcher` to the mock. For a real server set `KNOWLEDGE_API_UPSTREAM` and either
`KNOWLEDGE_DEV_BEARER` or `KNOWLEDGE_DEV_COOKIE_FILE` (the latter contains the
Cookie header and is re-read per request). These credentials stay in the dev
proxy; the SPA sends only same-origin session cookies. Never put credentials in
`VITE_*` environment variables.

The mock implements the C4 routes under `/api/knowledge/v1`, with `{data:[]}`
lists and direct single-resource responses. It starts with a `handbook`
collection and one document. State resets on process restart. Markdown writes
upsert by collection/externalId, split into 1000-character chunks, and use
256-dimensional deterministic feature hashing plus lexical scores and RRF.
This is a UI fixture, not a substitute for PostgreSQL/pgvector proof. Sync
enqueues a job on one FIFO worker: it stays queued for 0.5 s, then runs for
2.5 s, upserting documents one by one (`GET /api/knowledge/v1/jobs/{id}` reads
it). Notion sync creates clearly labelled local documents, bounded to 100 IDs,
without a known total; IDs beginning `error:` exercise partial errors and an ID
beginning `fail:` fails the job. esa sync creates two labelled posts
(`esa:<team>:<number>`) against a total of 2; a query containing `error:` adds a
partial error and one containing `fail:` fails the job. No Notion, esa or model
API is called. Search with `expand` returns `expanded: ["年次有給休暇", "休暇申請"]`. `KNOWLEDGE_MOCK_NOTION_PLATFORM_MANAGED=1` /
`KNOWLEDGE_MOCK_ESA_PLATFORM_MANAGED=1` report the respective token as
platform-managed and reject supplied tokens with 400
`knowledge_notion_token_platform_managed` / `knowledge_esa_token_platform_managed`.
`GET /api/knowledge/v1/providers/esa` reports `{platformManaged:false}` by default
and, in esa platform-managed mode, a connection that reads the `docs` and
`engineering` teams (the Sources form then offers them as a team select).
`KNOWLEDGE_MOCK_ESA_CONNECTION_ERROR=1` implies esa platform management and
reports a rejected token (`connected:false`, `error: "esa returned HTTP 401"`).
Mock answers are labelled as mock and streamed with citations first;
`KNOWLEDGE_MOCK_LLM=0` disables Ask, rerank and expand and returns 503
`knowledge_llm_unavailable`. `KNOWLEDGE_MOCK_BACKFILL=1` makes `/config` report a
re-embed backfill of 48 documents from its first read: queued for 1 s, then 8
fewer every 2 s until idle.

| Flag | Effect |
|---|---|
| `KNOWLEDGE_MOCK_PORT` | Mock listen port (default 5206) |
| `KNOWLEDGE_MOCK_LLM=0` | No LLM: no Ask, rerank or expand (503) |
| `KNOWLEDGE_MOCK_NOTION_PLATFORM_MANAGED=1` | Notion token managed by the Project |
| `KNOWLEDGE_MOCK_ESA_PLATFORM_MANAGED=1` | esa token managed by the Project; `docs`/`engineering` teams |
| `KNOWLEDGE_MOCK_ESA_CONNECTION_ERROR=1` | esa platform-managed with a rejected token |
| `KNOWLEDGE_MOCK_BACKFILL=1` | Re-embed backfill countdown on `/config` |

## Checks

From `web/`:

```sh
bun run typecheck
bun run test
bun run build:image
```

`build:image` writes `web/dist/`; `build` first typechecks.
Tests cover search, job and config DTO parsing, fragmented UTF-8/CRLF SSE,
error and truncated streams, mock CRUD/upserts/token redaction, the sync job
lifecycle (on a test clock), expand and backfill, and each route rendered with
mock API data in both locales. Browser screenshots belong in ignored `.tmp/`.

The client reports server errors without silently converting them to empty
results. Cancelling Ask or leaving its screen aborts the answer request; a stream
without `done` is an error, not a successful partial answer.


## Server and image

The server and all of its internal packages live in `cmd/server/`. It requires
`github.com/appthrust/appserver-go v0.2.0` without a replacement or platform
checkout. PostgreSQL with pgvector stores Knowledge data; configure
`KNOWLEDGE_DATABASE_URL` (or `DATABASE_URL`) and the `PLATFORM_*` identity and API
settings before running a real integration. `KNOWLEDGE_EMBEDDING_URL` and
`KNOWLEDGE_LLM_URL` are supplied by the catalog's automatic `embedding` and
`ai-gateway` dependencies. The access-key audience remains `knowledge`.

From the repository root:

```sh
go vet ./...
go build ./... && go test ./...
bun test hack/
go run ./cmd/server --static-dir web/dist
```

PostgreSQL integration tests are opt-in: set
`BUILTIN_APPSERVER_TEST_DATABASE_URL` to a disposable pgvector-enabled database.
Tests create isolated schemas, and the role/extension privilege test requires a
superuser test DSN. Without a test database, the integration cases skip.

The Docker image builds the Vite SPA and Go binary separately, then serves the
SPA from `/srv/knowledge` using `/knowledge-server` as user `65532` on port
`8080`. No JavaScript runtime is included in the final distroless image.

```sh
NODE_AUTH_TOKEN="$(gh auth token)" docker build --platform linux/amd64 \
  --secret id=NODE_AUTH_TOKEN,env=NODE_AUTH_TOKEN -t knowledge-local .
```

## Release

Once the bootstrap prerequisites below are complete, create and push a new
SemVer tag, starting with `v0.2.0` (the existing catalog floor owns `0.1.0`):

```sh
git tag v0.2.0
git push origin v0.2.0
```

`.github/workflows/publish.yml` builds Linux amd64 and arm64, pushes the index to
`ghcr.io/appthrust/knowledge:0.2.0`, resolves its immutable digest, and signs that
digest with Sigstore keyless signing. Both the source repository and the published image must remain public. `GITHUB_TOKEN` needs `packages:write` and OIDC needs
`id-token:write`. GitHub Packages must grant this repository read access to
`@appthrust/app-launcher` and `@appthrust/platform-dashboard-i18n`; npm credentials
enter the Docker build only as a BuildKit `NODE_AUTH_TOKEN` secret.

Configure repository secret `CLOUD_CATALOG_ADMIN_TOKEN` and, if needed, variable
`CLOUD_API_URL` (default `https://api.appthrust.com`). Grant this repository access
to organization secrets `APPTHRUST_GITHUB_ACTIONS_APP_ID` and
`APPTHRUST_GITHUB_ACTIONS_APP_PRIVATE_KEY`. The GitHub App must be installed for
`appthrust/cloud` with Contents write permission to send repository dispatches.
Cloud's API needs no GitHub credential.

Publication has two phases:

```sh
bun hack/publish-entry.ts submit --version 0.2.0 --digest sha256:<image-index-digest>
# If submit reports state=snapshot, dispatch with an installation token for Cloud:
gh api -X POST repos/appthrust/cloud/dispatches -f event_type=catalog-publish -f 'client_payload[snapshot_id]=<snapshot-id>'
bun hack/publish-entry.ts await --version 0.2.0 --digest sha256:<image-index-digest>
```

`submit` reads the existing Cloud entry, merges its version history with
`catalog/entry.yaml`, and PUTs the complete `CatalogEntry` to
`/api/v1/catalog/entries/knowledge`. It then POSTs `/api/v1/catalog/publish`,
accepting either HTTP 202 `{id, state: "queued"}` (Cloud dispatched publication)
or HTTP 200 `{id, state: "snapshot"}` (the immutable snapshot needs dispatch).
It writes `snapshot_id` and `state` to `$GITHUB_OUTPUT` in Actions and exits
without polling. For `snapshot` only, the workflow mints a short-lived GitHub App
installation token scoped to `appthrust/cloud` and dispatches
the `catalog-publish` repository event with that snapshot id. For `queued`, it skips dispatch.
Cloud's workflow requires its own `CLOUD_CATALOG_API_URL` repository variable and
`CLOUD_CATALOG_ADMIN_TOKEN` secret to fetch the snapshot.

`await` requires no Cloud admin token. Dispatch is not success: every 15 seconds
for at most 10 minutes it fetches layer 0 (`index.json`) of
`ghcr.io/appthrust/catalog:latest` through ORAS, checks its digest, and requires
the exact Knowledge version and image digest. Before succeeding it verifies
that immutable catalog's signature against issuer
`https://token.actions.githubusercontent.com` and identity
`https://github.com/appthrust/cloud/.github/workflows/catalog-publish.yml@refs/heads/main`.
Timeout, API failure, dispatch failure, or an unverified artifact fails the release.
ORAS 1.3.0 and Cosign 3.1.3 are installed by the workflow; manual publication
needs them too.

## Catalog and image contract

- `catalog/entry.yaml` is the complete ApplicationStackDefinition. Its versions
  section is the publisher's append point, not an image tag substitution.
- The seed `0.1.0` retains its original
  `ghcr.io/appthrust/platform/knowledge@sha256:...` image. All subsequent versions
  use `ghcr.io/appthrust/knowledge@sha256:<index-digest>`, `stream: <major.minor>`,
  `served: true`, and `default: true`. Earlier versions stay served with their
  defaults cleared. The definition and top-level catalog versions agree.
- A version's images are immutable. Repeating the same version/digest is
  idempotent; attempting to replace its digest fails. The release workflow
  serializes Knowledge writers to protect its read/merge/PUT sequence.
- Changing from the seed's repository requires an explicit installation version
  update with `replaceRepository: true`. Afterwards the `follow` policy follows
  its stream; `pinned` requires an explicit version update.
- `/releasez` reports catalog-rendered `KNOWLEDGE_RELEASE_VERSION` and
  `KNOWLEDGE_ARTIFACT_DIGEST`; no platform release closure is involved.

## Registry access

`@appthrust/app-launcher` and `@appthrust/platform-dashboard-i18n` are public
GitHub Packages owned by `appthrust/platform`. GitHub Packages still requires
a token for downloads: CI and the publisher use this repository's
`GITHUB_TOKEN` as `NODE_AUTH_TOKEN` (`web/.npmrc`); GitHub App installation
tokens are rejected by the npm registry (403). Locally:
`NODE_AUTH_TOKEN="$(gh auth token)" bun install` in `web/`.
`web/bun.lock` is registry-resolved and committed; CI and Docker use
`--frozen-lockfile`.

The committed compatibility floor is the complete spec read from
`ApplicationStackDefinition/knowledge` on `appthrust-ink-pmc001` on 2026-11-06.
It preserves the served `0.1.0` version and image, both automatic dependencies,
and the access-key audience and scopes verbatim. The first independent release
is `0.2.0` in stream `0.2`; no placeholder image is committed for it.

Architecture and migration authority:
[`first-party-app-repositories.md`](https://github.com/appthrust/platform/blob/main/design/platform/marketplace-apps/first-party-app-repositories.md).
Imported from platform commit `27347914ee15817f27a42055160b42506edc73e6`,
`cmd/knowledge-server/` and `dashboard/packages/knowledge/`; this README retains
the SPA's operating guide. The publication workflow follows `appthrust/directory`.
