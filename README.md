# Flake

Flake distributes clean Lean verification builds across trusted machines. It runs
Lean modules on remote workers and leaves the artifacts and traces in the
reviewer's ordinary Lake workspace. The first supported release is Lean 4.35.0
on Linux with matching master and worker architectures.

## Build and run

With Go 1.26, Python 3, and Elan available, run `scripts/build`. It builds the
dedicated Lake adapter from the official toolchain's installed source and places
the commands in `bin/`. Set `FLAKE_LEAN_ROOT` to build from an existing Lean 4.35.0
installation. The Docker image packages the same commands.

Put `bin` on the master PATH. In a Lean 4.35.0 project, create a worker config.

```toml
listen = "0.0.0.0:7431"
workers = 2
wait = "10s"
events = "/tmp/flake-events.jsonl"
```

Start each worker on the trusted network. Give each host a distinct identity.

```sh
flake-worker --connect MASTER:7431 --slots 4 --toolchain /opt/lean
```

Then run the build from the reviewer workspace.

```sh
flake --flake-config=workers.toml build Mathlib
lake --no-build build Mathlib
```

Leading Flake options come before Lake arguments. `--flake-policy` accepts
`eager`, `delayed`, or `single`. The initial configuration uses `eager`.
`adapter` in the TOML can point to a dedicated Lake binary when it is packaged
separately from Flake. Commands such as `flake env` and builds without a worker
config run official Lake directly.

For a cold build, Flake sends one compressed snapshot of the root project's
tracked regular files to each worker while Lake prepares local dependencies.
The snapshot includes resources read by Mathlib's linters and widgets. Use
matching Flake binaries on the master and workers. Compilation
inputs and outputs are checked by digest. Workers use the selected toolchain
and a relocated project tree. Root module actions using files outside the
workspace and toolchain, or compiler arguments with unsupported paths, go
through the local compiler. Use trusted projects and workers on a private
network.

## Verification

```sh
go test ./...
scripts/test-compose
```

The Compose suite starts two workers for cross-host scheduling, then three for
eager replication. It also checks argument forwarding, Lean failures, the
resulting Lake paths, and a local import after the distributed build exits.

To run the pinned Mathlib benchmark, start the same worker processes before
every clustered run and record their host specifications. On the master, run
the following command with a new output directory.

```sh
scripts/benchmark /absolute/path/to/workers.toml /absolute/path/to/run
```

Pass a third argument such as `Mathlib.Init` to validate the benchmark on a subtree.
The runner prepares the pinned dependency sources once. It copies the prepared
workspace for each run and compiles from source with external and shared Lake
caches disabled. It compares official Lake and the three Flake policies. Logs,
per-attempt events, build timings, and the summary table stay in the output
directory. The Flake wall-clock figure ends when Lake finishes and the artifacts
are local. CPU and utilization account for attempts that report process timing.
The table identifies attempts whose CPU time was unavailable after disconnects.
