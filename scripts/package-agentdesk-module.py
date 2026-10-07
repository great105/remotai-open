"""Package a portable AgentDesk build, preserving all original dependencies."""
import argparse
import hashlib
import json
from pathlib import Path
import zipfile

parser = argparse.ArgumentParser()
parser.add_argument("--input", type=Path, required=True)
parser.add_argument("--output", type=Path, required=True)
args = parser.parse_args()
folder = args.input.resolve()
assert (folder / "AgentDeskBridge.exe").is_file()
metadata = folder / "_internal/module-version.json"
version = json.loads(metadata.read_text(encoding="utf-8"))
assert version["version"] == "1.0.0" and len(version["adapter_sha256"]) == 64
args.output.parent.mkdir(parents=True, exist_ok=True)
with zipfile.ZipFile(args.output, "x", compression=zipfile.ZIP_DEFLATED, compresslevel=4) as archive:
    for path in sorted(folder.rglob("*")):
        if path.is_file():
            assert not path.is_symlink()
            archive.write(path, (Path(folder.name) / path.relative_to(folder)).as_posix())
    archive.writestr("README.txt", "Remotai local speech module 1.0.0 (Windows x64)\n\nExtract the complete folder on the computer that runs Remotai.\nSettings > Local capabilities > Connect a local module: choose the extracted folder.\nKeep AgentDeskBridge.exe and _internal together. Download speech models in Remotai.\nModels and optional engines are downloaded separately; existing AgentDesk caches are reused.\n")
with zipfile.ZipFile(args.output) as archive:
    assert archive.testzip() is None
with args.output.open("rb") as stream:
    digest = hashlib.file_digest(stream, "sha256").hexdigest()
proof = {"filename": args.output.name, "sha256": digest, "size": args.output.stat().st_size, "module": version}
args.output.with_suffix(".json").write_text(json.dumps(proof, indent=2) + "\n", encoding="utf-8")
print(json.dumps(proof))
