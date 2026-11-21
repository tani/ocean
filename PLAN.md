# Flake

## Objective

Flake helps people reviewing large Lean 4 formalizations check a fixed revision from source faster. As AI-assisted mathematics produces larger formalizations, reproducible Lean checks could become part of the infrastructure for peer review in theoretical research. Flake uses speculative parallel execution, including redundant module compilations, to shorten the time to a verified build. The primary measure is elapsed time from starting a clean verification build to completing Lake's checks and publishing the artifacts on the reviewer's machine.

## Current goal

Use Mathlib as the current verification workload. Build the full `Mathlib` target with `flake [<extra-flags>] build Mathlib` across multiple hosts. Start from a pinned checkout with its specified Lean toolchain and an empty cache for Mathlib module outputs. Compare the time to checked artifacts with a clean `lake build Mathlib` on one machine. The master can reuse the resulting artifacts with ordinary Lake after the workers stop. [Mathlib](https://github.com/leanprover-community/mathlib4)

## Drop-in Lake CLI

- Replace `lake <args>` with `flake [<extra-flags>] <args>`. Consume leading Flake options in the `--flake-*` namespace, then forward every remaining argument to Lake in order and with its original boundaries. For example, `flake --flake-config=workers.toml --dir=./proof build MyTarget` distributes eligible jobs for `lake --dir=./proof build MyTarget`. [Lake CLI reference](https://lean-lang.org/doc/reference/latest/Build-Tools-and-Distribution/Lake/#command-line-interface)
- Use the caller's working directory, Lake toolchain selection, and existing package configuration. Preserve standard input, output, error output, and exit status when delegating to Lake. A project can adopt Flake by changing the command it runs.
- Distribute eligible compilation jobs within the Lake invocation. Pass commands such as `lake --help` and `lake env` through to Lake and run unsupported build actions locally. With no compatible workers, run the ordinary Lake invocation locally.

## Build outputs

Report success only after every `.olean` required by the requested targets is available as a local file on the master, including artifacts built on slaves and prerequisites resolved locally. Place each file where the equivalent local `lake build` would make it available under the same workspace configuration. Publish the companion artifacts and traces that ordinary Lake needs to use the outputs. The completed build remains usable on the master after all slaves shut down.

## Master–slave architecture

Implement the Flake master, slave, and integration test harness in Go.

- **Master.** A single master runs alongside Lake in the reviewer's workspace or in verification CI. It constructs the file-level graph from Lake's resolved imports and uses Lake's up-to-date decisions. It schedules ready files, sends their inputs to workers, and validates and publishes returned artifacts where Lake expects them. [Lake build reference](https://lean-lang.org/doc/reference/latest/Build-Tools-and-Distribution/Lake/#builds)
- **Workers (slaves).** A configured pool of workers advertises toolchain, platform, and available slots to the master. Each worker runs assigned compiler jobs in separate directories and returns outputs, diagnostics, and exit status. All job requests and results go through the master.

## CBOR protocol

- Encode all master–slave messages in CBOR. For the first release, each slave opens a persistent TCP connection to the master. Prefix each CBOR message with its byte length and enforce a maximum message size. The master handles worker connections concurrently. [RFC 8949](https://www.rfc-editor.org/rfc/rfc8949.html)
- Use CBOR maps for RPC requests and replies covering worker registration, capacity updates, job assignment, completion, and cancellation. Match replies by request ID and use a separate attempt ID for each execution of a build job. Return compiler exit status and diagnostics as job results. Report invalid requests as protocol errors.
- Transfer source files and build artifacts as bounded chunks of CBOR byte strings. Include the file size and digest so the receiver can verify the complete file before it is used or published. [CBOR byte strings](https://www.rfc-editor.org/rfc/rfc8949.html#section-3.1)

## File-level dependency graph

- Starting from the requested Lake targets, walk resolved direct imports and create one node per reachable `.lean` file. Record which imported files each node needs and which files depend on it. Shared imports have one node with multiple dependents, so the dependency tree is represented as a directed acyclic graph (DAG).
- Mark an import satisfied when its Lake-validated artifacts are available, whether from a local build, a previous build, or a worker. In the first release, imports from dependency packages and the toolchain enter the graph as prerequisites fulfilled locally.
- Queue each file as soon as all of its imports and other prerequisites are satisfied. When a file's artifacts are published, use its reverse edges to release newly ready files immediately. Independent branches can run concurrently, and each dependent waits only for its own inputs.

## Speculative scheduling

- Rank ready modules by their longest remaining dependency chain, measured in modules at first. Use recorded compile and transfer times to identify which modules and worker assignments most affect the overall finish time. [LeanBuild scheduling](https://github.com/palladin/lean-cloud/blob/main/Applications/LeanBuild/README.md#how-it-runs)
- Choose between starting a new module and starting another attempt of a module already assigned. For a module likely to hold up the build, the master can start several attempts on different compatible hosts as soon as its inputs are ready. It can give a replica priority over a lower-priority ready module when the replica is expected to bring the build finish time forward. Size the fan-out to the available worker capacity and account for input and output transfers when more attempts would compete for bandwidth.
- Each attempt uses the same verified inputs and effective compiler configuration and writes to its own staging area. The master accepts the first complete result to pass validation, publishes its artifact set once, and releases dependents once. Cancel other attempts where possible and discard any results that arrive after publication.
- A failed attempt leaves any other active attempt eligible to finish. If all attempts fail, use the transport retry or local fallback policy where applicable and report the final compilation error through Lake.

## Artifact reuse within a verification build

- Study Mathlib's `lake exe cache get` for resolving the transitive imports of requested modules and computing module keys from package configuration, sources, and import hashes. Use the resolved graph from Lake and key Flake jobs by their effective toolchain, platform, compiler inputs, source, and imported artifacts. Track each transferred file's content digest separately from the job key. [Mathlib `Cache/Hashing.lean`](https://github.com/leanprover-community/mathlib4/blob/master/Cache/Hashing.lean)
- Apply the cache's missing-only downloads and overlapping download and unpack stages to CBOR transfers. Send the worker an input manifest and transfer only blobs it lacks. Reuse verified inputs in later jobs within the same build and overlap independent transfers with compilation. [Mathlib `Cache/Requests.lean`](https://github.com/leanprover-community/mathlib4/blob/master/Cache/Requests.lean)
- Use Mathlib's per-module `.ltar` archives and `leantar` restore as a reference for grouping `.olean` files, companion outputs, and traces. Test whether the pinned toolchain's `leantar` can restore a worker-built bundle in a separate master workspace and leave it reusable by plain `lake build`. Use Lake's actual output paths for each package. [Mathlib `Cache/IO.lean`](https://github.com/leanprover-community/mathlib4/blob/master/Cache/IO.lean)

## Prior work

- **LeanBuild in `lean-cloud`.** This implementation distributes a Lean module DAG across workers, keeps one Lake session per worker, prioritizes longer dependency chains, and overlaps artifact transfers with compilation. It uses blob storage to share artifacts and restores them on one worker before asking Lake to finish the original targets. Its AWS notes report a source build of 9,031 modules in 25m27s on four 8-core ARM64 hosts with Lean 4.35.0, excluding setup and worker startup. Use its Lake integration and pinned Mathlib workload as references, and measure Flake's own single-machine and distributed baselines. [Architecture](https://github.com/palladin/lean-cloud/blob/main/Applications/LeanBuild/README.md#how-it-runs), [AWS build report](https://github.com/palladin/lean-cloud/blob/main/Applications/LeanBuild/MathlibAWS.md)
- **`rules_lean` and `turbo-lean`.** The Bazel rules show how to declare one action per Lean module with explicit toolchain, source, and imported artifact inputs. The `turbo-lean` module builder is a reference for DAG construction and hashing `.olean` files together with their companion artifacts. Use these to check Flake's job inputs and output manifests. [Bazel `rules_lean`](https://github.com/atvrager/rules_lean), [`turbo-lean` module builder](https://github.com/Th0rgal/turbo-lean/blob/main/crates/turbo-lean/src/module_builder.rs)
- **Romik, *Optimizing the Lean and Mathlib toolchain* (2026).** The paper demonstrates a `lean` executable wrapper used by Lake and discusses the freshness risks when the wrapper changes compiler inputs that Lake does not track. Verify that Flake's integration preserves effective compiler inputs and Lake's freshness checks. [Paper, section 3.8](https://danromik.com/resources/papers/leanopt.pdf#page=26)
- **Huch and Wenzel, *Distributed Parallel Build for the Isabelle Archive of Formal Proofs* (ITP 2024).** This proof-library build study uses measured runtimes and critical paths to schedule work on a heterogeneous cluster. Use its evaluation approach when comparing elapsed build time, CPU consumption, and resource limits on Mathlib. [Paper, sections 3–4](https://doi.org/10.4230/LIPIcs.ITP.2024.22)
- **Proactive task cloning and LATE.** Ananthanarayanan et al. evaluated launching replicas at task start for small data-processing jobs. Zaharia et al. used estimated time remaining to select jobs for delayed replication. Use the two policies as reference points for the Mathlib comparison. [Attack of the Clones (NSDI 2013)](https://www.usenix.org/conference/nsdi13/technical-sessions/presentation/ananthanarayanan), [LATE (OSDI 2008)](https://www.usenix.org/legacy/event/osdi08/tech/full_papers/zaharia/zaharia_html/)

## First release

- Support one pinned Lean toolchain, one Linux architecture, and one Lake workspace per invocation. Start with trusted workers.
- Offload standard Lean module compilation in the root package. Run dependency package builds, custom targets, native compilation, and linking locally.

## Milestones

1. **Prove the Lake integration.** Pin the Mathlib revision and toolchain. Compare intercepting Lake's compiler invocation with LeanBuild's version-specific helper, which uses `imports.fetch` to discover dependencies and `leanArts.fetch` to build modules. For one delegated module, verify that the chosen integration exposes resolved imports and effective inputs, preserves the drop-in CLI, and leaves reusable artifacts after the master publishes them. [LeanBuild's Lake helper](https://github.com/palladin/lean-cloud/blob/main/Applications/LeanBuild/Helper.lean), [Lake build actions](https://lean-lang.org/doc/api/Lake/Build/Actions.html)
2. **Run one file on one worker.** Implement the Go master and slave programs and verify a CBOR request and response round trip for one compilation. Have the master send the source, module setup, compiler arguments, relevant environment, and imported artifacts for the job. Use matching toolchains and stage inputs in a job-specific directory. Check which paths must be mapped between the master and worker. Return compiler diagnostics, exit status, and every artifact Lake expects. Validate and stage the outputs before publishing them for dependent jobs.
3. **Schedule files and replicas across workers.** Build the file-level graph and ready queue on the master. Add the drop-in `flake` command and worker service, then implement both immediate and delayed replicas with single publication of each job's outputs. Retry transport failures within a fixed limit, then run the job locally if no worker is available. Discard partial or late results from abandoned attempts.
4. **Verify with Docker Compose.** Add the Compose topology and run the integration cases below. Use controlled worker delays to test that eager replicas can release a blocked dependency chain earlier.
5. **Measure the Mathlib verification build.** Start with a representative subtree, then compile the full `Mathlib` target from source. Compare single-machine `lake build Mathlib` with Flake policies that permit one attempt, delayed replicas, and eager replicas. Use the same worker pool for the Flake runs and match the revision, toolchain, target, and initial dependency cache state in every run. Measure elapsed time through artifact publication. Report CPU time spent on discarded attempts, total CPU time, worker utilization, master ingress and egress, and peak memory. Report host specifications, setup time, and which dependencies were built from source. Compare with the published LeanBuild workload under matched conditions when available.

## Docker Compose integration tests

- Define one `master` container and a scalable `slave` service. Use `docker compose up --build --scale slave=2` for the baseline. Each slave registers with the master and uses the same pinned Lean toolchain. [Docker Compose up reference](https://docs.docker.com/reference/cli/docker/compose/up/)
- Keep the Lake workspace in the master and give each slave its own filesystem. Have the Go test harness launch the drop-in command and start the baseline build once the required slaves register. Transfer source files and artifacts as CBOR messages to exercise the same protocol used across machines.
- Use a fixture where `Left.lean` and `Right.lean` both import `Base.lean`, and `Main.lean` imports both branches. Capture dispatch, completion, and artifact publication events per file and worker so tests can assert ordering and overlap. Delay or disconnect a slave during a job to exercise duplicate attempts and recovery.

## Acceptance criteria

- On the same fixture, replacing `lake` with `flake` and adding leading Flake flags preserves Lake's working-directory and target selection. Test a Lake global option before `build`, multiple targets, and arguments containing spaces or passed after a Lake `--`. Compare pass-through commands and no-worker fallback with ordinary Lake for output and exit status.
- In the Compose fixture, `Base.lean` satisfies both branches. `Left.lean` and `Right.lean` compile concurrently on different slaves. `Main.lean` starts after both branches publish their artifacts, and the build passes the ordinary Lake checks.
- After the distributed build succeeds, stop all slave containers. Compare the master's `.olean` files for the requested targets against the paths produced by an equivalent local Lake build. Run plain `lake build` on the master and verify it reuses the artifacts without recompiling those modules. Compile a new module on the master that imports the distributed results.
- A Compose run with three slaves starts three attempts of the same ready module before a worker is classified as slow. Force one replica to finish first and verify that its validated outputs release the downstream jobs exactly once. Late results leave the published artifacts intact.
- A worker disconnect or interrupted transfer leaves no usable partial result. Retrying or falling back locally allows the build to finish.
- The CBOR receiver reconstructs messages across split or combined TCP reads. Concurrent replies match the right requests and job attempts. An artifact transferred in multiple CBOR byte string chunks matches its declared size and digest before publication.
- A Lean compilation error returns usable diagnostics and a failing exit status. A worker with a mismatched toolchain receives no jobs.
- In the Mathlib milestone, each benchmark run compiles the Mathlib modules from source. In the distributed run, compiler activity overlaps on at least two worker hosts and shared inputs are reused across jobs. After all workers stop, ordinary `lake build Mathlib` reuses the master's artifacts and a new local file can `import Mathlib`.

## Long-term goal

After the Mathlib milestone, measure clean verification builds of larger fixed Lean developments from source. Use each project's pinned toolchain and documented Lake verification targets. Reference workloads include Tau Ceti, Anthropic's Fermat's Last Theorem formalization, and OpenAI's Navier–Stokes formalization. [Tau Ceti](https://github.com/TauCetiProject/TauCeti), [Anthropic's FLT](https://github.com/anthropics/fermats-last-theorem), [OpenAI's Navier–Stokes and Euler formalizations](https://github.com/openai/NavierStokesAndEuler)
