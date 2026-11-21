"""Add Ocean's graph and compiler hooks to Lean 4.35.0's Lake sources."""

import pathlib
import sys

root = pathlib.Path(sys.argv[1]) / "Lake"


def replace(file, original, replacement):
    path = root / file
    text = path.read_text()
    if text.count(original) != 1:
        raise SystemExit(f"unexpected Lake sources in {path}")
    path.write_text(text.replace(original, replacement))


replace("Build/Actions.lean", 'public def compileLeanModule\n',
        'public def oceanCompileLeanModule\n')
replace("Build/Actions.lean", '  (leanir : FilePath := "leanir")',
        '''  (leanir : FilePath := "leanir")
  (oceanRoot : Option FilePath := none)''')
replace("Build/Actions.lean", '''  let out ← rawProc {
    args
    cmd := lean.toString
    env := #[
      ("LEAN_PATH", leanPath.toString)
    ]
  }''', '''  let compiler : IO.Process.SpawnArgs := {
    args, cmd := lean.toString, env := #[("LEAN_PATH", leanPath.toString)]
  }
  let runLocal := rawProc compiler
  let out ← do
    let some root := oceanRoot | runLocal
    let some bridge ← IO.getEnv "OCEAN_BRIDGE" | runLocal
    let input := Json.mkObj [
      ("root", toJson root), ("cwd", toJson (← IO.currentDir)),
      ("module", toJson setup.name.toString), ("source", toJson leanFile),
      ("setup", toJson setupFile), ("args", toJson args),
      ("leanPath", toJson leanPath), ("compiler", toJson lean)]
    let result ← IO.Process.output {cmd := bridge, args := #["--ocean-internal-compile"]}
      (some input.compress) |>.toBaseIO
    let .ok result := result | runLocal
    if result.exitCode != 0 then runLocal else
      match Json.parse result.stdout with
      | .error _ => runLocal
      | .ok json =>
        match json.getObjValAs? Nat "exit", json.getObjValAs? String "stdout",
            json.getObjValAs? String "stderr" with
        | .ok exitCode, .ok stdout, .ok stderr => pure {exitCode := exitCode.toUInt32, stdout, stderr}
        | _, _, _ => runLocal''')
replace("Build/Actions.lean", "public def compileO", '''public def compileLeanModule
  (leanFile relLeanFile : FilePath)
  (setup : ModuleSetup) (setupFile : FilePath)
  (arts : ModuleArtifacts)
  (leanArgs : Array String := #[])
  (leanPath : SearchPath := [])
  (lean : FilePath := "lean")
  (leanir : FilePath := "leanir")
: LogIO Unit :=
  oceanCompileLeanModule leanFile relLeanFile setup setupFile arts leanArgs leanPath lean leanir

public def compileO''')
replace("Build/Module.lean", '''  compileLeanModule presetup.srcFile relSrcFile setup mod.setupFile arts args
    (← getLeanPath) (← getLean) (← getLeanir)''', '''  oceanCompileLeanModule presetup.srcFile relSrcFile setup mod.setupFile arts args
    (← getLeanPath) (← getLean) (← getLeanir)
    (if mod.pkg.wsIdx == 0 then some mod.pkg.dir else none)''')

replace("CLI/Main.lean", "protected def build : CliM PUnit := do", '''private def oceanRoots (ws : Workspace) (key : BuildKey) : FetchM (Array Lake.Module) := do
  match key with
  | .module name => pure <| (ws.findTargetModule? name).toArray
  | .packageModule pkg name =>
    pure <| ((ws.findPackageByKey? pkg).bind (·.findTargetModule? name)).toArray
  | .packageTarget pkg name =>
    if let some p := ws.findPackageByKey? pkg then
      if let some lib := p.findLeanLib? name then
        (← lib.extraDep.fetch).await
        return ← lib.getModuleArray
    return #[]
  | .facet target _ =>
    match target with
    | .module name => pure <| (ws.findTargetModule? name).toArray
    | .packageModule pkg name =>
      pure <| ((ws.findPackageByKey? pkg).bind (·.findTargetModule? name)).toArray
    | .packageTarget pkg name =>
      if let some p := ws.findPackageByKey? pkg then
        if let some lib := p.findLeanLib? name then
          (← lib.extraDep.fetch).await
          return ← lib.getModuleArray
      return #[]
    | _ => pure #[]
  | _ => pure #[]

private partial def oceanVisit (mod : Lake.Module)
  (graph : NameMap (Array String)) : FetchM (NameMap (Array String)) := do
  if mod.pkg.wsIdx != 0 || graph.contains mod.name then return graph
  (← mod.lib.extraDep.fetch).await
  let imports ← (← mod.imports.fetch).await
  let imports := imports.filter (·.pkg.wsIdx == 0)
  imports.foldlM (fun graph imp => oceanVisit imp graph)
    (graph.insert mod.name (imports.map (·.name.toString)))

protected def build : CliM PUnit := do''')
replace("CLI/Main.lean", '''  ws.runBuild (buildSpecs specs) buildConfig

protected def checkBuild''', '''  ws.runBuild (do
    if let some bridge ← IO.getEnv "OCEAN_BRIDGE" then
      let roots ← specs.foldlM (fun acc spec => return acc ++ (← oceanRoots ws spec.info.key)) #[]
      let graph ← roots.foldlM (fun acc mod => oceanVisit mod acc) {}
      let mut preloadRoot := ""
      for mod in roots do
        if mod.pkg.wsIdx == 0 && !(← mod.oleanFile.pathExists) then
          preloadRoot := ws.root.dir.toString
          break
      let modules := Json.mkObj <| graph.toList.map fun (name, imports) =>
        (name.toString, toJson imports)
      let data := Json.mkObj [("root", toJson preloadRoot), ("cwd", toJson (← IO.currentDir)),
        ("graph", modules)]
      discard <| (IO.Process.output {cmd := bridge, args := #["--ocean-internal-graph"]}
        (some data.compress)).toBaseIO
    buildSpecs specs) buildConfig

protected def checkBuild''')

replace("Config/InstallPath.lean", '''  let elan? ← findElanInstall?
  if let some sysroot ← findLakeLeanJointHome? then''', '''  let elan? ← findElanInstall?
  if (← IO.getEnv "OCEAN_BRIDGE").isSome then
    if let some lean ← findLeanInstall? then
      return (elan?, some lean, some (LakeInstall.ofLean lean))
  if let some sysroot ← findLakeLeanJointHome? then''')
