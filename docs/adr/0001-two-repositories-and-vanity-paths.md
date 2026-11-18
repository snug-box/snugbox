# Decision: two repositories, with Go module paths under snugbox.dev

Status: proposed
Date: 2026-10-31
Deciders: the owner, on the build's proposal

## Context

The agent is Apache-2.0 and runs on customers' servers. The hub, the dashboard and the website are AGPL-3.0 and run as a service (blueprint section 9.1). The Go import path is written into every file that imports a package, and into every program that depends on the agent, so changing it later means a new major version. The GitHub organisation name `snugbox` was taken on 31 October 2026, and the owner hasn't chosen another.

## Decision

Two repositories, `snugbox` for the agent and `hub` for the rest, each a Go module: `snugbox.dev/snugbox` and `snugbox.dev/hub`. A `go.work` file outside both lets them be built together locally.

## Options considered

- One repository for everything. Simpler to change across both at once, but it mixes two licences in one tree, and the blueprint chose two.
- Module paths under `github.com/devpilotX`. Work today, but break the day the code moves to an organisation, and every importer has to change.
- Module paths under `github.com/snugbox`. Not possible: the name belongs to someone else.
- Doing nothing about the path until an organisation exists. Every package written in the meantime would need its imports rewritten.

## Consequences

`go get snugbox.dev/snugbox` works only once snugbox.dev is bought and serves a `go-import` meta tag naming the repository. Until then the module builds from a clone, and CI uses its own checkout. The repository can move between GitHub accounts without changing a line of Go.

## Reversal

Wrong if the domain can't be had. Then the path has to change before the first release, which is a search and replace across the code and `go.mod`; after the first release it would mean a new major version.
