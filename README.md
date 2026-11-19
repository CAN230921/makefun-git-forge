# Git Forge

[![make.fun bounty](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01)
[![Rank](https://make.fun/badge/github-core-clone-01/CAN230921/makefun-git-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard)
[![Reward](https://make.fun/badge/github-core-clone-01/CAN230921/makefun-git-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01)

Git Forge is a single-process, GitHub-compatible server for Git Smart HTTP,
REST v3, GraphQL, and MCP. All four interfaces use the same in-memory store.
Restarting the process clears its repositories and metadata.

## Run locally

Use Node.js 22. The server has no external dependencies.

```sh
FORGE_TOKEN=local-demo-token FORGE_OWNER=local FORGE_PORT=3000 node main.ts
curl -X POST http://127.0.0.1:3000/api/v3/user/repos \
  -H 'Authorization: Bearer local-demo-token' \
  -H 'Content-Type: application/json' \
  --data '{"name":"example","auto_init":true}'
git -c 'http.extraHeader=Authorization: Bearer local-demo-token' \
  clone http://127.0.0.1:3000/local/example.git
```

The HTTP listener defaults to loopback. The REST API starts at `/api/v3`,
GraphQL at `/api/graphql`, MCP at `/mcp`, and the unauthenticated health check
at `/healthz`.

## Bounty runner

When the grader supplies `MAKEFUN_ENV_URL` and `MAKEFUN_SHOP_TOKEN`, the
server binds an ephemeral port, registers with the grader, and exits after the
grader reports completion.

The contest scope covers Git SHA-1 repositories, branches and tags,
repositories and file contents, issues and comments, pull requests and squash
merges, the corresponding GraphQL schema, and six MCP tools.
